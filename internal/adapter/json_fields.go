package adapter

import (
	"encoding/json"
	"errors"
)

type jsonSpan struct {
	start int
	end   int
}

// jsonObjectFields indexes validated top-level values without copying their payloads.
func jsonObjectFields(body []byte) (map[string]jsonSpan, error) {
	if !json.Valid(body) {
		return nil, errors.New("请求 JSON 无效")
	}
	pos := skipJSONSpace(body, 0)
	if body[pos] != '{' {
		return nil, errors.New("请求必须是有效的 JSON 对象")
	}
	fields := make(map[string]jsonSpan)
	pos = skipJSONSpace(body, pos+1)
	for body[pos] != '}' {
		keyEnd := skipJSONString(body, pos)
		var key string
		if err := json.Unmarshal(body[pos:keyEnd], &key); err != nil {
			return nil, err
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("请求包含重复的 JSON 字段")
		}
		pos = skipJSONSpace(body, keyEnd)
		start := skipJSONSpace(body, pos+1)
		end := skipJSONValue(body, start)
		fields[key] = jsonSpan{start: start, end: end}
		pos = skipJSONSpace(body, end)
		if body[pos] == ',' {
			pos = skipJSONSpace(body, pos+1)
		}
	}
	return fields, nil
}

func skipJSONSpace(body []byte, pos int) int {
	for pos < len(body) {
		switch body[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		default:
			return pos
		}
	}
	return pos
}

func skipJSONString(body []byte, pos int) int {
	for pos++; pos < len(body); pos++ {
		switch body[pos] {
		case '\\':
			pos++
		case '"':
			return pos + 1
		}
	}
	return len(body)
}

func skipJSONValue(body []byte, pos int) int {
	switch body[pos] {
	case '"':
		return skipJSONString(body, pos)
	case '{', '[':
		depth := 1
		for pos++; depth > 0; pos++ {
			switch body[pos] {
			case '"':
				pos = skipJSONString(body, pos) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		return pos
	default:
		for pos < len(body) {
			switch body[pos] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return pos
			}
			pos++
		}
		return pos
	}
}

func replaceJSONSpan(body []byte, span jsonSpan, value []byte) []byte {
	size := len(body) + len(value) - (span.end - span.start)
	if size <= cap(body) {
		if size > len(body) {
			body = body[:size]
		}
		copy(body[span.start+len(value):], body[span.end:])
		copy(body[span.start:], value)
		return body[:size]
	}
	out := make([]byte, size)
	copy(out, body[:span.start])
	copy(out[span.start:], value)
	copy(out[span.start+len(value):], body[span.end:])
	return out
}
