package qoder

import "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

// FormatQoder is the Qoder protocol format identifier.
const FormatQoder translator.Format = "qoder"

// QoderRequest represents a Qoder API request.
type QoderRequest struct {
	Model    string         `json:"model"`
	Messages []QoderMessage `json:"messages"`
	Stream   bool           `json:"stream,omitempty"`
}

// QoderMessage represents a single message in a Qoder request.
type QoderMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// QoderResponse represents a Qoder API response.
type QoderResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message QoderMessage `json:"message"`
	} `json:"choices"`
}
