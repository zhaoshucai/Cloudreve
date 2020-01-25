package oss

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	model "github.com/HFO4/cloudreve/models"
	"github.com/HFO4/cloudreve/pkg/filesystem/fsctx"
	"github.com/HFO4/cloudreve/pkg/filesystem/response"
	"github.com/HFO4/cloudreve/pkg/request"
	"github.com/HFO4/cloudreve/pkg/serializer"
	"github.com/HFO4/cloudreve/pkg/util"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"io"
	"net/url"
	"path"
	"time"
)

// UploadPolicy 阿里云OSS上传策略
type UploadPolicy struct {
	Expiration string        `json:"expiration"`
	Conditions []interface{} `json:"conditions"`
}

// CallbackPolicy 回调策略
type CallbackPolicy struct {
	CallbackURL      string `json:"callbackUrl"`
	CallbackBody     string `json:"callbackBody"`
	CallbackBodyType string `json:"callbackBodyType"`
}

// Driver 阿里云OSS策略适配器
type Driver struct {
	Policy     *model.Policy
	client     *oss.Client
	bucket     *oss.Bucket
	HTTPClient request.Client
}

type key int

const (
	// VersionID 文件版本标识
	VersionID key = iota
)

// InitOSSClient 初始化OSS鉴权客户端
func (handler *Driver) InitOSSClient() error {
	if handler.Policy == nil {
		return errors.New("存储策略为空")
	}

	if handler.client == nil {
		// 初始化客户端
		client, err := oss.New(handler.Policy.Server, handler.Policy.AccessKey, handler.Policy.SecretKey)
		if err != nil {
			return err
		}
		handler.client = client

		// 初始化存储桶
		bucket, err := client.Bucket(handler.Policy.BucketName)
		if err != nil {
			return err
		}
		handler.bucket = bucket

	}

	return nil
}

// Get 获取文件
func (handler Driver) Get(ctx context.Context, path string) (response.RSCloser, error) {
	// 通过VersionID禁止缓存
	ctx = context.WithValue(ctx, VersionID, time.Now().UnixNano())

	// 获取文件源地址
	downloadURL, err := handler.Source(
		ctx,
		path,
		url.URL{},
		int64(model.GetIntSetting("preview_timeout", 60)),
		false,
		0,
	)
	if err != nil {
		return nil, err
	}

	// 获取文件数据流
	resp, err := handler.HTTPClient.Request(
		"GET",
		downloadURL,
		nil,
		request.WithContext(ctx),
	).CheckHTTPResponse(200).GetRSCloser()
	if err != nil {
		return nil, err
	}

	resp.SetFirstFakeChunk()

	// 尝试自主获取文件大小
	if file, ok := ctx.Value(fsctx.FileModelCtx).(model.File); ok {
		resp.SetContentLength(int64(file.Size))
	}

	return resp, nil
}

// Put 将文件流保存到指定目录
func (handler Driver) Put(ctx context.Context, file io.ReadCloser, dst string, size uint64) error {
	defer file.Close()

	// 初始化客户端
	if err := handler.InitOSSClient(); err != nil {
		return err
	}

	// 凭证有效期
	credentialTTL := model.GetIntSetting("upload_credential_timeout", 3600)

	options := []oss.Option{
		oss.Expires(time.Now().Add(time.Duration(credentialTTL) * time.Second)),
	}

	// 上传文件
	err := handler.bucket.PutObject(dst, file, options...)
	if err != nil {
		return err
	}

	return nil
}

// Delete 删除一个或多个文件，
// 返回未删除的文件
func (handler Driver) Delete(ctx context.Context, files []string) ([]string, error) {
	// 初始化客户端
	if err := handler.InitOSSClient(); err != nil {
		return files, err
	}

	// 删除文件
	delRes, err := handler.bucket.DeleteObjects(files)

	if err != nil {
		return files, err
	}

	// 统计未删除的文件
	failed := util.SliceDifference(files, delRes.DeletedObjects)
	if len(failed) > 0 {
		return failed, errors.New("删除失败")
	}

	return []string{}, nil
}

// Thumb 获取文件缩略图
func (handler Driver) Thumb(ctx context.Context, path string) (*response.ContentResponse, error) {
	// 初始化客户端
	if err := handler.InitOSSClient(); err != nil {
		return nil, err
	}

	var (
		thumbSize = [2]uint{400, 300}
		ok        = false
	)
	if thumbSize, ok = ctx.Value(fsctx.ThumbSizeCtx).([2]uint); !ok {
		return nil, errors.New("无法获取缩略图尺寸设置")
	}

	thumbParam := fmt.Sprintf("image/resize,m_lfit,h_%d,w_%d", thumbSize[1], thumbSize[0])
	thumbOption := []oss.Option{oss.Process(thumbParam)}
	thumbURL, err := handler.signSourceURL(
		ctx,
		path,
		int64(model.GetIntSetting("preview_timeout", 60)),
		thumbOption,
	)
	if err != nil {
		return nil, err
	}

	return &response.ContentResponse{
		Redirect: true,
		URL:      thumbURL,
	}, nil
}

// Source 获取外链URL
func (handler Driver) Source(
	ctx context.Context,
	path string,
	baseURL url.URL,
	ttl int64,
	isDownload bool,
	speed int,
) (string, error) {
	// 初始化客户端
	if err := handler.InitOSSClient(); err != nil {
		return "", err
	}

	// 尝试从上下文获取文件名
	fileName := ""
	if file, ok := ctx.Value(fsctx.FileModelCtx).(model.File); ok {
		fileName = file.Name
	}

	// 添加各项设置
	var signOptions = make([]oss.Option, 0, 2)
	if isDownload {
		signOptions = append(signOptions, oss.ResponseContentDisposition("attachment; filename=\""+url.PathEscape(fileName)+"\""))
	}
	if speed > 0 {
		// OSS对速度值有范围限制
		if speed < 819200 {
			speed = 819200
		}
		if speed > 838860800 {
			speed = 838860800
		}
		signOptions = append(signOptions, oss.TrafficLimitParam(int64(speed)))
	}

	return handler.signSourceURL(ctx, path, ttl, signOptions)
}

func (handler Driver) signSourceURL(ctx context.Context, path string, ttl int64, options []oss.Option) (string, error) {
	cdnURL, err := url.Parse(handler.Policy.BaseURL)
	if err != nil {
		return "", err
	}

	// 公有空间不需要签名
	if !handler.Policy.IsPrivate {
		file, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		sourceURL := cdnURL.ResolveReference(file)
		return sourceURL.String(), nil
	}

	signedURL, err := handler.bucket.SignURL(path, oss.HTTPGet, ttl, options...)
	if err != nil {
		return "", err
	}

	// 将最终生成的签名URL域名换成用户自定义的加速域名（如果有）
	finalURL, err := url.Parse(signedURL)
	if err != nil {
		return "", err
	}
	finalURL.Host = cdnURL.Host
	finalURL.Scheme = cdnURL.Scheme

	return finalURL.String(), nil
}

// Token 获取上传策略和认证Token
func (handler Driver) Token(ctx context.Context, TTL int64, key string) (serializer.UploadCredential, error) {
	// 读取上下文中生成的存储路径
	savePath, ok := ctx.Value(fsctx.SavePathCtx).(string)
	if !ok {
		return serializer.UploadCredential{}, errors.New("无法获取存储路径")
	}

	// 生成回调地址
	siteURL := model.GetSiteURL()
	apiBaseURI, _ := url.Parse("/api/v3/callback/oss/" + key)
	apiURL := siteURL.ResolveReference(apiBaseURI)

	// 回调策略
	callbackPolicy := CallbackPolicy{
		CallbackURL:      apiURL.String(),
		CallbackBody:     `{"name":${x:fname},"source_name":${object},"size":${size},"pic_info":"${imageInfo.width},${imageInfo.height}"}`,
		CallbackBodyType: "application/json",
	}

	// 上传策略
	postPolicy := UploadPolicy{
		Expiration: time.Now().UTC().Add(time.Duration(TTL) * time.Second).Format(time.RFC3339),
		Conditions: []interface{}{
			map[string]string{"bucket": handler.Policy.BucketName},
			[]string{"starts-with", "$key", path.Dir(savePath)},
			[]interface{}{"content-length-range", 0, handler.Policy.MaxSize},
		},
	}

	return handler.getUploadCredential(ctx, postPolicy, callbackPolicy, TTL)
}

func (handler Driver) getUploadCredential(ctx context.Context, policy UploadPolicy, callback CallbackPolicy, TTL int64) (serializer.UploadCredential, error) {
	// 读取上下文中生成的存储路径
	savePath, ok := ctx.Value(fsctx.SavePathCtx).(string)
	if !ok {
		return serializer.UploadCredential{}, errors.New("无法获取存储路径")
	}

	// 处理回调策略
	callbackPolicyEncoded := ""
	if callback.CallbackURL != "" {
		callbackPolicyJSON, err := json.Marshal(callback)
		if err != nil {
			return serializer.UploadCredential{}, err
		}
		callbackPolicyEncoded = base64.StdEncoding.EncodeToString(callbackPolicyJSON)
		policy.Conditions = append(policy.Conditions, map[string]string{"callback": callbackPolicyEncoded})
	}

	// 编码上传策略
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return serializer.UploadCredential{}, err
	}
	policyEncoded := base64.StdEncoding.EncodeToString(policyJSON)

	// 签名上传策略
	hmacSign := hmac.New(sha1.New, []byte(handler.Policy.SecretKey))
	_, err = io.WriteString(hmacSign, policyEncoded)
	if err != nil {
		return serializer.UploadCredential{}, err
	}
	signature := base64.StdEncoding.EncodeToString(hmacSign.Sum(nil))

	return serializer.UploadCredential{
		Policy:    fmt.Sprintf("%s:%s", callbackPolicyEncoded, policyEncoded),
		Path:      savePath,
		AccessKey: handler.Policy.AccessKey,
		Token:     signature,
	}, nil
}
