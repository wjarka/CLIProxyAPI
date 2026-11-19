package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func (s *Service) request(ctx context.Context, method, path, token, zaiToken string, input, output any) (status, code int, err error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, 0, ErrInvalid
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return 0, 0, ErrUpstream
	}
	if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = "Bearer " + token
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if zaiToken != "" {
		req.Header.Set("X-Bigmodel-Authorization", zaiToken)
		req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, ErrUpstream
	}
	defer resp.Body.Close()

	const maxResponse = 64 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, &envelope) != nil || envelope.Code == nil {
		return resp.StatusCode, 0, ErrUpstream
	}
	if *envelope.Code != 0 || resp.StatusCode != http.StatusOK {
		return resp.StatusCode, *envelope.Code, nil
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) || json.Unmarshal(envelope.Data, output) != nil {
		return resp.StatusCode, 0, ErrUpstream
	}
	return resp.StatusCode, 0, nil
}

func resetError(status, code int, err error) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrReconnect
	}
	if err != nil || status != http.StatusOK || code != 0 {
		return ErrUpstream
	}
	return nil
}
