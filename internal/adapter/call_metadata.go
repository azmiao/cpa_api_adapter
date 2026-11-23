package adapter

import (
	"encoding/json"
	"strconv"
)

// Observe only routing and usage scalars. Text, images and tool arguments never
// enter a log buffer; nesting and captured scalar sizes have fixed bounds.
type jsonCallMetadata struct {
	Type     string
	Model    string
	Status   string
	Code     string
	HasError bool
	Usage    callUsage
}

type callJSONField uint8

const (
	callFieldNone callJSONField = iota
	callFieldType
	callFieldModel
	callFieldStatus
	callFieldResponse
	callFieldUsage
	callFieldError
	callFieldCode
	callFieldInput
	callFieldOutput
	callFieldTotal
	callFieldCached
	callFieldDetails
)
const (
	callScopeIgnored uint8 = iota
	callScopeRoot
	callScopeResponse
	callScopeUsage
	callScopeDetails
	callScopeError
)

type callJSONFrame struct {
	scope uint8
	array bool
	state uint8 // key, colon, value, comma
	field callJSONField
}

type jsonCallScanner struct {
	meta      jsonCallMetadata
	frames    [64]callJSONFrame
	depth     int
	token     uint8 // string or primitive
	key       bool
	escaped   bool
	hasEscape bool
	capture   bool
	overflow  bool
	value     [512]byte
	length    int
	started   bool
	done      bool
	invalid   bool
}

func (s *jsonCallScanner) reset() {
	*s = jsonCallScanner{meta: jsonCallMetadata{Usage: emptyCallUsage()}}
}

func callField(scope uint8, key string) callJSONField {
	switch scope {
	case callScopeRoot, callScopeResponse:
		switch key {
		case "type":
			return callFieldType
		case "model":
			return callFieldModel
		case "status":
			return callFieldStatus
		case "response":
			if scope == callScopeRoot {
				return callFieldResponse
			}
		case "usage":
			return callFieldUsage
		case "error":
			return callFieldError
		case "code":
			return callFieldCode
		}
	case callScopeUsage:
		switch key {
		case "input_tokens", "prompt_tokens":
			return callFieldInput
		case "output_tokens", "completion_tokens":
			return callFieldOutput
		case "total_tokens":
			return callFieldTotal
		case "input_tokens_details", "prompt_tokens_details":
			return callFieldDetails
		}
	case callScopeDetails:
		if key == "cached_tokens" {
			return callFieldCached
		}
	case callScopeError:
		if key == "code" || key == "type" {
			return callFieldCode
		}
	}
	return callFieldNone
}

func (s *jsonCallScanner) appendValue(c byte) {
	if !s.capture {
		return
	}
	if s.length == len(s.value) {
		s.overflow = true
		return
	}
	s.value[s.length] = c
	s.length++
}

func (s *jsonCallScanner) finishToken() {
	frame := &s.frames[s.depth-1]
	if s.key {
		frame.field = callFieldNone
		if !s.overflow && s.capture {
			data := s.value[:s.length]
			var key string
			if s.hasEscape {
				if json.Unmarshal(data, &key) != nil {
					s.invalid = true
					return
				}
			} else {
				key = string(data[1 : len(data)-1])
			}
			frame.field = callField(frame.scope, key)
		}
		frame.state = 1
	} else {
		frame.state = 3
		if !s.overflow && s.capture {
			data := s.value[:s.length]
			if s.token == 1 {
				var value string
				if json.Unmarshal(data, &value) != nil {
					s.invalid = true
					return
				}
				value = boundedLogString(value)
				switch frame.field {
				case callFieldModel:
					s.meta.Model = value
				case callFieldType:
					s.meta.Type = value
				case callFieldStatus:
					s.meta.Status = value
				case callFieldCode:
					s.meta.Code = value
				}
			} else if n, err := strconv.ParseInt(string(data), 10, 64); err == nil && n >= 0 {
				switch frame.field {
				case callFieldInput:
					s.meta.Usage.Input = tokenCount(n)
				case callFieldOutput:
					s.meta.Usage.Output = tokenCount(n)
				case callFieldTotal:
					s.meta.Usage.Total = tokenCount(n)
				case callFieldCached:
					s.meta.Usage.Cached = tokenCount(n)
				}
			}
		}
	}
	s.token, s.length = 0, 0
}

func (s *jsonCallScanner) feed(data []byte) {
	if !s.started {
		s.reset()
		s.started = true
	}
	for _, c := range data {
		if s.invalid {
			return
		}
		if s.token == 1 {
			s.appendValue(c)
			if s.escaped {
				s.escaped = false
				continue
			}
			if c == '\\' {
				s.escaped, s.hasEscape = true, true
				continue
			}
			if c == '"' {
				s.finishToken()
			}
			continue
		}
		if s.token == 2 {
			if c != ',' && c != '}' && c != ']' && c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				s.appendValue(c)
				continue
			}
			s.finishToken()
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		if s.depth == 0 {
			if s.done || c != '{' {
				s.invalid = true
				return
			}
			s.frames[0] = callJSONFrame{scope: callScopeRoot}
			s.depth = 1
			continue
		}
		frame := &s.frames[s.depth-1]
		switch c {
		case '"':
			if frame.state != 0 && frame.state != 2 {
				s.invalid = true
				return
			}
			s.key = frame.state == 0 && !frame.array
			s.token, s.length, s.overflow, s.escaped = 1, 0, false, false
			s.hasEscape = false
			s.capture = s.key && frame.scope != callScopeIgnored || !s.key && (frame.field == callFieldType || frame.field == callFieldModel || frame.field == callFieldStatus || frame.field == callFieldCode)
			s.appendValue(c)
		case ':':
			if frame.state != 1 {
				s.invalid = true
				return
			}
			frame.state = 2
		case ',':
			if frame.state != 3 {
				s.invalid = true
				return
			}
			frame.field = callFieldNone
			if frame.array {
				frame.state = 2
			} else {
				frame.state = 0
			}
		case '{', '[':
			if frame.state != 2 || s.depth == len(s.frames) {
				s.invalid = true
				return
			}
			scope := callScopeIgnored
			if c == '{' {
				switch frame.field {
				case callFieldResponse:
					scope = callScopeResponse
				case callFieldUsage:
					scope = callScopeUsage
				case callFieldDetails:
					scope = callScopeDetails
				case callFieldError:
					scope = callScopeError
					s.meta.HasError = true
				}
			}
			frame.state = 3
			s.frames[s.depth] = callJSONFrame{scope: scope, array: c == '['}
			if c == '[' {
				s.frames[s.depth].state = 2
			}
			s.depth++
		case '}', ']':
			if frame.array != (c == ']') || frame.state == 1 || !frame.array && frame.state == 2 {
				s.invalid = true
				return
			}
			s.depth--
			if s.depth == 0 {
				s.done = true
			}
		default:
			if frame.state != 2 {
				s.invalid = true
				return
			}
			s.key, s.token, s.length, s.overflow = false, 2, 0, false
			s.capture = frame.scope == callScopeUsage || frame.scope == callScopeDetails
			s.appendValue(c)
		}
	}
}

func (s *jsonCallScanner) complete() bool { return s.done && !s.invalid && s.token == 0 }

func scanCallMetadata(data []byte) jsonCallMetadata {
	var scanner jsonCallScanner
	scanner.feed(data)
	if scanner.complete() {
		return scanner.meta
	}
	return jsonCallMetadata{Usage: emptyCallUsage()}
}

type sseCallScanner struct {
	parser        jsonCallScanner
	prefix        [5]byte
	lineLength    int
	dataLine      bool
	optionalSpace bool
	hasData       bool
	doneLength    int
	doneMatch     bool
}

func (s *sseCallScanner) payload(c byte) {
	s.parser.feed([]byte{c})
	if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
		return
	}
	if s.doneLength >= len("[DONE]") || c != "[DONE]"[s.doneLength] {
		s.doneMatch = false
	}
	s.doneLength++
}

func (s *sseCallScanner) dispatch(trace *callTrace) {
	if s.hasData {
		if s.parser.complete() {
			trace.observe(s.parser.meta)
		}
		if s.doneMatch && s.doneLength == len("[DONE]") {
			trace.markTerminal()
		}
	}
	s.parser.reset()
	s.hasData, s.doneLength, s.doneMatch = false, 0, true
}

func (s *sseCallScanner) feed(data []byte, trace *callTrace) {
	if !s.parser.started && !s.hasData && s.lineLength == 0 {
		s.doneMatch = true
	}
	for _, c := range data {
		if c == '\n' {
			if s.lineLength == 0 || s.lineLength == 1 && s.prefix[0] == '\r' {
				s.dispatch(trace)
			}
			if s.dataLine {
				s.payload('\n')
			}
			s.lineLength, s.dataLine, s.optionalSpace = 0, false, false
			continue
		}
		if s.dataLine {
			if s.optionalSpace {
				s.optionalSpace = false
				if c == ' ' {
					s.lineLength++
					continue
				}
			}
			s.payload(c)
		} else if s.lineLength < len(s.prefix) {
			s.prefix[s.lineLength] = c
			if s.lineLength == 4 && string(s.prefix[:]) == "data:" {
				s.dataLine, s.optionalSpace, s.hasData = true, true, true
			}
		}
		s.lineLength++
	}
}

func (s *sseCallScanner) end(trace *callTrace) { s.dispatch(trace) }
