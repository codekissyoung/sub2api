package service

import "github.com/gin-gonic/gin"

// OpenAIForwardAdjustedWrittenSize 返回扣除心跳填充字节后的已写响应大小。
// 非流式 JSON 空白心跳（images 与 /responses 非流式共用同一实现）优先，其次
// compact/流式 SSE 注释心跳。快照与比较必须使用同一口径，否则心跳字节会被
// 误判为语义输出而放弃 failover 换号（#3887）。
func OpenAIForwardAdjustedWrittenSize(c *gin.Context) int {
	if OpenAIImagesJSONKeepalivePresent(c) {
		return OpenAIImagesJSONKeepaliveAdjustedWrittenSize(c)
	}
	return OpenAICompactKeepaliveAdjustedWrittenSize(c)
}
