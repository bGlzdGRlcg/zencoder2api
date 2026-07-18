package service

import (
	"bufio"
	"io"
	"net/http"
)

// StreamResponse 流式传输响应到客户端
func StreamResponse(w http.ResponseWriter, resp *http.Response) error {
	// 复制响应头
	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 获取Flusher接口
	flusher, ok := w.(http.Flusher)
	if !ok {
		// 如果不支持Flusher，直接复制
		_, err := io.Copy(w, resp.Body)
		return err
	}

	// 使用bufio读取并逐行刷新
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			_, writeErr := w.Write(line)
			if writeErr != nil {
				return writeErr
			}
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// CopyResponse 普通响应复制
func CopyResponse(w http.ResponseWriter, resp *http.Response) error {
	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err := io.Copy(w, resp.Body)
	return err
}
