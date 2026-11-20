package adapter

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"os"
	"strings"
	"sync"
)

const memoryMultipartLimit = 1 << 20

var uploadBuffers = sync.Pool{New: func() any {
	buffer := make([]byte, 32<<10)
	return &buffer
}}

func readRequestBody(r *http.Request, limit int64) ([]byte, error) {
	reader := io.LimitReader(r.Body, limit+1)
	var body []byte
	var err error
	if r.ContentLength >= 0 && r.ContentLength <= limit {
		body = make([]byte, int(r.ContentLength))
		_, err = io.ReadFull(reader, body)
		if err != nil {
			return nil, &apiError{http.StatusBadRequest, "请求体读取失败"}
		}
		var extra [1]byte
		if n, readErr := reader.Read(extra[:]); n > 0 {
			tail, tailErr := io.ReadAll(reader)
			err = tailErr
			body = append(body, extra[0])
			body = append(body, tail...)
		} else if readErr != io.EOF {
			err = readErr
		}
	} else {
		body, err = io.ReadAll(reader)
	}
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, "请求体读取失败"}
	}
	if int64(len(body)) > limit {
		return nil, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"}
	}
	return body, nil
}

func prepareRequestBody(r *http.Request, api APIConfig, limit int64) (Model, bool, func(), error) {
	original := r.Body
	defer original.Close()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Header.Get("Content-Type") == "" || err == nil && mediaType == "application/json" {
		body, err := readRequestBody(r, limit)
		if err != nil {
			return Model{}, false, nil, err
		}
		body, mapping, changed, err := rewriteRequest(r, api, body)
		if err != nil {
			return Model{}, false, nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Length")
		return mapping, changed, func() {}, nil
	}
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return Model{}, false, nil, &apiError{http.StatusUnsupportedMediaType, "模型白名单接口仅支持 JSON 或 multipart/form-data"}
	}
	return prepareMultipartBody(r, api, limit, params["boundary"])
}

func spoolMultipartBody(r *http.Request, limit int64) (io.ReaderAt, int64, func(), error) {
	if r.ContentLength >= 0 && r.ContentLength <= memoryMultipartLimit {
		body, err := readRequestBody(r, limit)
		if err != nil {
			return nil, 0, nil, err
		}
		return bytes.NewReader(body), int64(len(body)), func() {}, nil
	}
	file, err := os.CreateTemp("", "cpa-adapter-upload-*")
	if err != nil {
		return nil, 0, nil, &apiError{http.StatusInternalServerError, "无法暂存上传请求"}
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	buffer := uploadBuffers.Get().(*[]byte)
	size, err := io.CopyBuffer(struct{ io.Writer }{file}, io.LimitReader(r.Body, limit+1), *buffer)
	uploadBuffers.Put(buffer)
	if err != nil {
		cleanup()
		return nil, 0, nil, &apiError{http.StatusBadRequest, "请求体读取失败"}
	}
	if size > limit {
		cleanup()
		return nil, 0, nil, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"}
	}
	return file, size, cleanup, nil
}

func multipartModel(source io.ReaderAt, size int64, boundary string, api APIConfig) (string, error) {
	reader := multipart.NewReader(io.NewSectionReader(source, 0, size), boundary)
	maxID := 0
	for _, mapping := range api.Mappings {
		if len(mapping.ID) > maxID {
			maxID = len(mapping.ID)
		}
	}
	var id string
	found := false
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", &apiError{http.StatusBadRequest, "multipart 请求无效"}
		}
		if part.FormName() != "model" {
			continue
		}
		if found || part.FileName() != "" {
			return "", &apiError{http.StatusBadRequest, "model 字段必须唯一且不能是文件"}
		}
		var valueReader io.Reader = part
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			valueReader = quotedprintable.NewReader(part)
		}
		value, err := io.ReadAll(io.LimitReader(valueReader, int64(maxID)+1))
		if err != nil {
			return "", &apiError{http.StatusBadRequest, "model 字段读取失败"}
		}
		if len(value) > maxID {
			return "", &apiError{http.StatusNotFound, "模型未对当前 API 开放"}
		}
		id, found = string(value), true
	}
	if !found {
		return "", &apiError{http.StatusBadRequest, "请求必须指定模型"}
	}
	return id, nil
}

func writeMultipartBody(destination io.Writer, source io.ReaderAt, size int64, boundary, target string) error {
	reader := multipart.NewReader(io.NewSectionReader(source, 0, size), boundary)
	writer := multipart.NewWriter(destination)
	if err := writer.SetBoundary(boundary); err != nil {
		return err
	}
	buffer := uploadBuffers.Get().(*[]byte)
	defer uploadBuffers.Put(buffer)
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return writer.Close()
		}
		if err != nil {
			return err
		}
		if part.FormName() == "model" {
			part.Header.Del("Content-Transfer-Encoding")
		}
		output, err := writer.CreatePart(part.Header)
		if err != nil {
			return err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(output, target)
		} else {
			_, err = io.CopyBuffer(output, part, *buffer)
		}
		if err != nil {
			return err
		}
	}
}

func prepareMultipartBody(r *http.Request, api APIConfig, limit int64, boundary string) (Model, bool, func(), error) {
	source, size, cleanup, err := spoolMultipartBody(r, limit)
	if err != nil {
		return Model{}, false, nil, err
	}
	id, err := multipartModel(source, size, boundary, api)
	if err != nil {
		cleanup()
		return Model{}, false, nil, err
	}
	mapping, changed, err := resolveModel(r, api, id)
	if err != nil {
		cleanup()
		return Model{}, false, nil, err
	}
	if !changed {
		r.Body = io.NopCloser(io.NewSectionReader(source, 0, size))
		r.ContentLength = size
	} else if size <= memoryMultipartLimit {
		var output bytes.Buffer
		output.Grow(int(size) + len(mapping.Target))
		if err := writeMultipartBody(&output, source, size, boundary, mapping.Target); err != nil {
			cleanup()
			return Model{}, false, nil, &apiError{http.StatusBadRequest, "无法改写 multipart 请求"}
		}
		r.Body = io.NopCloser(bytes.NewReader(output.Bytes()))
		r.ContentLength = int64(output.Len())
	} else {
		validationWriter := multipart.NewWriter(io.Discard)
		if err := validationWriter.SetBoundary(boundary); err != nil {
			cleanup()
			return Model{}, false, nil, &apiError{http.StatusBadRequest, "multipart boundary 无效"}
		}
		reader, writer := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = writer.CloseWithError(writeMultipartBody(writer, source, size, boundary, mapping.Target))
		}()
		closeSource := cleanup
		cleanup = func() {
			_ = reader.Close()
			closeSource()
			<-done
		}
		r.Body = reader
		r.ContentLength = -1
	}
	r.Header.Del("Content-Length")
	return mapping, changed, cleanup, nil
}
