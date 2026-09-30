package qoder_claude

// ConvertClaudeToQoder converts a Claude-format request to Qoder format.
func ConvertClaudeToQoder(req []byte) ([]byte, error) {
	// Claude format is similar to OpenAI for basic messages
	return req, nil
}

// ConvertQoderToClaude converts a Qoder-format response to Claude format.
func ConvertQoderToClaude(resp []byte) ([]byte, error) {
	return resp, nil
}
