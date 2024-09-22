package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/alibaba/higress/plugins/wasm-go/extensions/frontend-gray/config"
	"github.com/alibaba/higress/plugins/wasm-go/extensions/frontend-gray/util"

	"github.com/alibaba/higress/plugins/wasm-go/pkg/wrapper"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"
)

func main() {
	wrapper.SetCtx(
		"frontend-gray",
		wrapper.ParseConfigBy(parseConfig),
		wrapper.ProcessRequestHeadersBy(onHttpRequestHeaders),
		wrapper.ProcessResponseHeadersBy(onHttpResponseHeader),
		wrapper.ProcessResponseBodyBy(onHttpResponseBody),
		wrapper.ProcessStreamingResponseBodyBy(onStreamingResponseBody),
	)
}

func parseConfig(json gjson.Result, grayConfig *config.GrayConfig, log wrapper.Log) error {
	// 解析json 为GrayConfig
	config.JsonToGrayConfig(json, grayConfig)
	log.Infof("Rewrite: %v, GrayDeployments: %v", json.Get("rewrite"), json.Get("grayDeployments"))
	return nil
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, grayConfig config.GrayConfig, log wrapper.Log) types.Action {
	if !util.IsGrayEnabled(grayConfig) {
		return types.ActionContinue
	}

	cookies, _ := proxywasm.GetHttpRequestHeader("cookie")
	path, _ := proxywasm.GetHttpRequestHeader(":path")
	fetchMode, _ := proxywasm.GetHttpRequestHeader("sec-fetch-mode")

	isPageRequest := util.IsPageRequest(fetchMode, path)
	hasRewrite := len(grayConfig.Rewrite.File) > 0 || len(grayConfig.Rewrite.Index) > 0
	grayKeyValueByCookie := util.ExtractCookieValueByKey(cookies, grayConfig.GrayKey)
	grayKeyValueByHeader, _ := proxywasm.GetHttpRequestHeader(grayConfig.GrayKey)
	// 优先从cookie中获取，否则从header中获取
	grayKeyValue := util.GetGrayKey(grayKeyValueByCookie, grayKeyValueByHeader, grayConfig.GraySubKey)
	// 如果有重写的配置，则进行重写
	if hasRewrite {
		// 禁止重新路由，要在更改Header之前操作，否则会失效
		ctx.DisableReroute()
	}

	// 删除Accept-Encoding，避免压缩， 如果是压缩的内容，后续插件就没法处理了
	_ = proxywasm.RemoveHttpRequestHeader("Accept-Encoding")
	_ = proxywasm.RemoveHttpRequestHeader("Content-Length")
	deployment := &config.Deployment{}

	preVersion, preUniqueClientId := util.GetXPreHigressVersion(cookies)
	// 客户端唯一ID，用于在按照比率灰度时候 客户访问黏贴
	uniqueClientId := grayKeyValue
	if uniqueClientId == "" {
		xForwardedFor, _ := proxywasm.GetHttpRequestHeader("X-Forwarded-For")
		uniqueClientId = util.GetRealIpFromXff(xForwardedFor)
	}

	// 如果没有配置比例，则进行灰度规则匹配
	if isPageRequest {
		log.Infof("grayConfig.TotalGrayWeight==== %v", grayConfig.TotalGrayWeight)
		if grayConfig.TotalGrayWeight > 0 {
			deployment = util.FilterGrayWeight(&grayConfig, preVersion, preUniqueClientId, uniqueClientId)
		} else {
			deployment = util.FilterGrayRule(&grayConfig, grayKeyValue)
		}
		log.Infof("index deployment: %v, path: %v, backend: %v, xPreHigressVersion: %s,%s", deployment, path, deployment.BackendVersion, preVersion, preUniqueClientId)
	} else {
		deployment = util.GetVersion(grayConfig, deployment, preVersion, isPageRequest)
	}
	proxywasm.AddHttpRequestHeader(config.XHigressTag, deployment.Version)

	ctx.SetContext(config.XPreHigressTag, deployment.Version)
	ctx.SetContext(grayConfig.BackendGrayTag, deployment.BackendVersion)
	ctx.SetContext(config.IsPageRequest, isPageRequest)
	ctx.SetContext(config.XUniqueClientId, uniqueClientId)

	rewrite := grayConfig.Rewrite
	if rewrite.Host != "" {
		proxywasm.ReplaceHttpRequestHeader("HOST", rewrite.Host)
	}

	if hasRewrite {
		rewritePath := path
		if isPageRequest {
			rewritePath = util.IndexRewrite(path, deployment.Version, grayConfig.Rewrite.Index)
		} else {
			rewritePath = util.PrefixFileRewrite(path, deployment.Version, grayConfig.Rewrite.File)
		}
		log.Infof("rewrite path: %s %s %v", path, deployment.Version, rewritePath)
		proxywasm.ReplaceHttpRequestHeader(":path", rewritePath)
	}

	return types.ActionContinue
}

func onHttpResponseHeader(ctx wrapper.HttpContext, grayConfig config.GrayConfig, log wrapper.Log) types.Action {
	if !util.IsGrayEnabled(grayConfig) {
		return types.ActionContinue
	}
	status, err := proxywasm.GetHttpResponseHeader(":status")
	contentType, _ := proxywasm.GetHttpResponseHeader("Content-Type")

	if grayConfig.Rewrite != nil && grayConfig.Rewrite.Host != "" {
		// 删除Content-Disposition，避免自动下载文件
		proxywasm.RemoveHttpResponseHeader("Content-Disposition")
	}

	isPageRequest, ok := ctx.GetContext(config.IsPageRequest).(bool)
	if !ok {
		isPageRequest = false // 默认值
	}

	if err != nil || status != "200" {
		if status == "404" {
			if grayConfig.Rewrite.NotFound != "" && isPageRequest {
				ctx.SetContext(config.IsNotFound, true)
				responseHeaders, _ := proxywasm.GetHttpResponseHeaders()
				headersMap := util.ConvertHeaders(responseHeaders)
				if _, ok := headersMap[":status"]; !ok {
					headersMap[":status"] = []string{"200"} // 如果没有初始化，设定默认值
				} else {
					headersMap[":status"][0] = "200" // 修改现有值
				}
				if _, ok := headersMap["content-type"]; !ok {
					headersMap["content-type"] = []string{"text/html"} // 如果没有初始化，设定默认值
				} else {
					headersMap["content-type"][0] = "text/html" // 修改现有值
				}
				// 删除 content-length 键
				delete(headersMap, "content-length")
				proxywasm.ReplaceHttpResponseHeaders(util.ReconvertHeaders(headersMap))
				ctx.BufferResponseBody()
				return types.ActionContinue
			} else {
				ctx.DontReadResponseBody()
			}
		}
		log.Errorf("error status: %s, error message: %v", status, err)
		return types.ActionContinue
	}

	// 删除content-length，可能要修改Response返回值
	proxywasm.RemoveHttpResponseHeader("Content-Length")

	if strings.HasPrefix(contentType, "text/html") || isPageRequest {
		// 不会进去Streaming 的Body处理
		ctx.BufferResponseBody()

		proxywasm.ReplaceHttpResponseHeader("Cache-Control", "no-cache, no-store")

		frontendVersion := ctx.GetContext(config.XPreHigressTag).(string)
		xUniqueClient := ctx.GetContext(config.XUniqueClientId).(string)

		// 设置前端的版本
		proxywasm.AddHttpResponseHeader("Set-Cookie", fmt.Sprintf("%s=%s,%s; Max-Age=%s; Path=/;", config.XPreHigressTag, frontendVersion, xUniqueClient, grayConfig.UserStickyMaxAge))
		// 设置后端的版本
		if util.IsBackendGrayEnabled(grayConfig) {
			backendVersion := ctx.GetContext(grayConfig.BackendGrayTag).(string)
			proxywasm.AddHttpResponseHeader("Set-Cookie", fmt.Sprintf("%s=%s; Max-Age=%s; Path=/;", grayConfig.BackendGrayTag, backendVersion, grayConfig.UserStickyMaxAge))
		}
	}
	return types.ActionContinue
}

func onHttpResponseBody(ctx wrapper.HttpContext, grayConfig config.GrayConfig, body []byte, log wrapper.Log) types.Action {
	if !util.IsGrayEnabled(grayConfig) {
		return types.ActionContinue
	}
	isPageRequest, ok := ctx.GetContext(config.IsPageRequest).(bool)
	if !ok {
		isPageRequest = false // 默认值
	}
	frontendVersion := ctx.GetContext(config.XPreHigressTag).(string)

	isNotFound, ok := ctx.GetContext(config.IsNotFound).(bool)
	if !ok {
		isNotFound = false // 默认值
	}

	if isPageRequest && isNotFound && grayConfig.Rewrite.Host != "" && grayConfig.Rewrite.NotFound != "" {
		client := wrapper.NewClusterClient(wrapper.RouteCluster{Host: grayConfig.Rewrite.Host})

		client.Get(strings.Replace(grayConfig.Rewrite.NotFound, "{version}", frontendVersion, -1), nil, func(statusCode int, responseHeaders http.Header, responseBody []byte) {
			proxywasm.ReplaceHttpResponseBody(responseBody)
			proxywasm.ResumeHttpResponse()
		}, 1500)
		return types.ActionPause
	}

	if isPageRequest {
		// 将原始字节转换为字符串
		newBody := string(body)

		// 收集需要插入的内容
		headInjection := strings.Join(grayConfig.Injection.Head, "\n")
		bodyFirstInjection := strings.Join(grayConfig.Injection.Body.First, "\n")
		bodyLastInjection := strings.Join(grayConfig.Injection.Body.Last, "\n")

		// 使用 strings.Builder 来提高性能
		var sb strings.Builder
		// 预分配内存，避免多次内存分配
		sb.Grow(len(newBody) + len(headInjection) + len(bodyFirstInjection) + len(bodyLastInjection))
		sb.WriteString(newBody)

		// 进行替换
		content := sb.String()
		content = strings.ReplaceAll(content, "</head>", fmt.Sprintf("%s\n</head>", headInjection))
		content = strings.ReplaceAll(content, "<body>", fmt.Sprintf("<body>\n%s", bodyFirstInjection))
		content = strings.ReplaceAll(content, "</body>", fmt.Sprintf("%s\n</body>", bodyLastInjection))

		// 最终结果
		newBody = content

		if err := proxywasm.ReplaceHttpResponseBody([]byte(newBody)); err != nil {
			return types.ActionContinue
		}
	}
	return types.ActionContinue
}

func onStreamingResponseBody(ctx wrapper.HttpContext, pluginConfig config.GrayConfig, chunk []byte, isLastChunk bool, log wrapper.Log) []byte {
	return chunk
}
