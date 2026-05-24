package provider

import "strings"

func ResolveOpenAIBaseURL(baseURL string) string {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL
}

func BuildOpenAIURL(baseURL, path string) string {
	baseURL = ResolveOpenAIBaseURL(baseURL)
	if path == "" {
		return baseURL
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return baseURL + path
}

func BuildOpenAIResponsesURL(baseURL string) string {
	return BuildOpenAIURL(baseURL, "/v1/responses")
}

func BuildOpenAIModelsURL(baseURL string) string {
	return BuildOpenAIURL(baseURL, "/v1/models")
}
