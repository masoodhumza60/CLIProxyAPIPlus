package qoder_openai

import (
	"encoding/json"
)

// ConvertOpenAIToQoder converts an OpenAI-format request to Qoder format.
func ConvertOpenAIToQoder(req []byte) ([]byte, error) {
	var openaiReq map[string]interface{}
	if err := json.Unmarshal(req, &openaiReq); err != nil {
		return nil, err
	}

	// OpenAI format is close enough to Qoder format for now
	// Just pass through with model mapping
	return req, nil
}

// ConvertQoderToOpenAI converts a Qoder-format response to OpenAI format.
func ConvertQoderToOpenAI(resp []byte) ([]byte, error) {
	// Pass through for now
	return resp, nil
}
