package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// decodeJSON consumes the entire request, including trailing whitespace, so a
// small first object cannot hide an oversized body or a second JSON value.
func decodeJSON(dec *json.Decoder, target any) error {
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("请求体只能包含一份 JSON")
	}
	return nil
}

func (s *Server) requestBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		s.fail(w, http.StatusRequestEntityTooLarge, "请求体超过大小限制")
		return
	}
	// Decoder errors may contain submitted values; do not echo them.
	s.fail(w, http.StatusBadRequest, "请求体格式无效")
}
