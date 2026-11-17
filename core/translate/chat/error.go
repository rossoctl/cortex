package chat

// EncodeError gives an error body in the chat-completions shape.
func (Protocol) EncodeError(status int, errType, message string) []byte {
	type body struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    *int   `json:"code"`
	}
	// The value holds no json.RawMessage, so marshal cannot fail.
	b, _ := marshal(struct {
		Error body `json:"error"`
	}{body{Message: message, Type: errType}})
	return b
}
