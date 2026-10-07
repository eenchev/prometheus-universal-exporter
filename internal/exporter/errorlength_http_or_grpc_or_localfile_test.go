//go:build !select_request_types || request_type_http || request_type_grpc || request_type_localfile

package exporter

// rememberedTexts are the texts the failure log remembers failures by.
func rememberedTexts(server *Server) []string {
	server.failures.mu.Lock()
	defer server.failures.mu.Unlock()
	var texts []string
	for _, st := range server.failures.entries {
		if st.err != "" {
			texts = append(texts, st.err)
		}
	}
	return texts
}
