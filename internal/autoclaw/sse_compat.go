// sse_compat.go autoclaw 复用 workbuddy 上游的 SSE 解析/透传，
// 通过包级别适配器引用 internal/upstream（避免跨包符号循环：upstream 不依赖 autoclaw）。
package autoclaw

import (
	"io"
	"net/http"

	"workbuddy2api/internal/upstream"
)

// AggregateJSON 读取完整 SSE 流聚合成 OpenAI chat.completion（复用 upstream.Aggregate）。
func AggregateJSON(r io.Reader) (map[string]any, error) {
	return upstream.Aggregate(r)
}

// StreamSSE 透传规范化 SSE 到 ResponseWriter（复用 upstream.Stream，保证恰好一个 [DONE]）。
func StreamSSE(w http.ResponseWriter, r io.Reader) error {
	return upstream.Stream(w, r)
}
