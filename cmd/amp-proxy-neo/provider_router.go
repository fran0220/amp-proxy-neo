package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fran0220/amp-proxy-neo/pkg/auth"
	"github.com/fran0220/amp-proxy-neo/pkg/provider"
	"github.com/fran0220/amp-proxy-neo/pkg/retry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// providerRouter dispatches /api/provider/{provider}/... LLM requests to the
// matching provider handler (Claude / OpenAI / Codex / Gemini), resolving the
// caller's credentials via the auth resolver. It is the neo-side equivalent of
// the legacy amp-proxy router.go, scoped to what amp CLI needs.
type providerRouter struct {
	claude          *provider.ClaudeHandler
	openai          *provider.OpenAIHandler
	openaiResponses *provider.OpenAIResponsesHandler
	codex           *provider.CodexHandler
	gemini          *provider.GeminiHandler
	geminiCLI       *provider.GeminiCLIHandler
	state           *appState
}

func newProviderRouter(s *appState) *providerRouter {
	retryer := retry.NewRetryer(s.cfg.Retry.MaxAttempts, s.cfg.Retry.InitialDelay)
	return &providerRouter{
		claude:          provider.NewClaudeHandler(s.cfg, retryer, s.reqLog),
		openai:          provider.NewOpenAIHandler(s.cfg, retryer, s.reqLog),
		openaiResponses: provider.NewOpenAIResponsesHandler(s.cfg, retryer, s.reqLog),
		codex:           provider.NewCodexHandler(s.cfg, retryer, s.reqLog),
		gemini:          provider.NewGeminiHandler(s.cfg, retryer, s.reqLog),
		geminiCLI:       provider.NewGeminiCLIHandler(s.cfg, retryer, s.reqLog),
		state:           s,
	}
}

// ServeHTTP dispatches one provider request. The caller is responsible for
// matching `/api/provider/` first; this method assumes the path is in scope.
func (rt *providerRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	path := r.URL.Path
	providerName := extractProvider(path)

	if r.Method != http.MethodPost {
		rt.fallbackUpstream(w, r)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		log.Errorf("[PROVIDER-ROUTER] failed to read body: %v", err)
		rt.fallbackUpstream(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	model := gjson.GetBytes(bodyBytes, "model").String()
	if model == "" && providerName == "google" {
		model = provider.ExtractGeminiModel(path)
	}
	if model == "" {
		log.Infof("[PROVIDER-ROUTER] no model in body: %s %s", r.Method, path)
		rt.fallbackUpstream(w, r)
		return
	}

	if target, redirected := rt.state.cfg.ResolveModelRedirect(model); redirected {
		log.Infof("[REDIRECT] %s -> %s", model, target)
		model = target
		bodyBytes, _ = sjson.SetBytes(bodyBytes, "model", model)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	credentials, resolvedRoute := rt.state.resolver.Resolve(r.Context(), providerName, model)
	log.Infof("[ROUTE] model=%s provider=%s route=%s resolved=%s", model, providerName, rt.state.cfg.ModelRoute(providerName, model), resolvedRoute)

	if resolvedRoute != auth.RouteAmp && credentials != nil && credentials.Valid() {
		routeLabel := resolvedRoute + "/" + credentials.Source
		switch providerName {
		case "anthropic":
			log.Infof("[%s] %s -> %s (Claude)", routeLabel, model, path)
			rt.state.reqLog.LogRequest(model, providerName, routeLabel, path, start)
			rt.claude.Handle(w, r, bodyBytes, credentials)
			return
		case "openai":
			rt.dispatchOpenAI(w, r, bodyBytes, credentials, model, path, resolvedRoute, routeLabel, start)
			return
		case "google":
			rt.state.reqLog.LogRequest(model, providerName, routeLabel, path, start)
			if resolvedRoute == auth.RouteLocal && credentials.Source == "gemini-file" {
				log.Infof("[%s] %s -> %s (Gemini CLI)", routeLabel, model, path)
				rt.geminiCLI.Handle(w, r, bodyBytes, credentials)
			} else {
				log.Infof("[%s] %s -> %s (Gemini)", routeLabel, model, path)
				rt.gemini.Handle(w, r, bodyBytes, credentials)
			}
			return
		}
	}

	log.Infof("[PROVIDER-ROUTER] %s -> %s (route=amp or no auth, fallback)", model, path)
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	rt.fallbackUpstream(w, r)
	rt.state.reqLog.LogRequest(model, "upstream", "UPSTREAM", path, start)
}

func (rt *providerRouter) dispatchOpenAI(w http.ResponseWriter, r *http.Request, body []byte, credentials *auth.ProviderAuth, model, path, resolvedRoute, routeLabel string, start time.Time) {
	rt.state.reqLog.LogRequest(model, "openai", routeLabel, path, start)
	isResponses := isResponsesAPIPath(path)
	if isResponses {
		if resolvedRoute == auth.RouteLocal && credentials.Source == "codex-file" {
			log.Infof("[%s] %s -> %s (Codex CLI Responses)", routeLabel, model, path)
			rt.codex.Handle(w, r, body, credentials)
		} else {
			log.Infof("[%s] %s -> %s (OpenAI Responses API)", routeLabel, model, path)
			rt.openaiResponses.Handle(w, r, body, credentials)
		}
		return
	}
	if resolvedRoute == auth.RouteLocal && credentials.Source == "codex-file" {
		log.Infof("[%s] %s -> %s (Codex CLI)", routeLabel, model, path)
		rt.codex.Handle(w, r, body, credentials)
		return
	}
	log.Infof("[%s] %s -> %s (OpenAI)", routeLabel, model, path)
	rt.openai.Handle(w, r, body, credentials)
}

func (rt *providerRouter) fallbackUpstream(w http.ResponseWriter, r *http.Request) {
	if rt.state.upstream != nil {
		rt.state.upstream.forward(w, r)
		return
	}
	http.Error(w, `{"error":"no upstream and no local credentials"}`, http.StatusBadGateway)
}

func extractProvider(path string) string {
	parts := strings.SplitN(path, "/", 5)
	if len(parts) >= 4 {
		return strings.ToLower(parts[3])
	}
	return ""
}

func isResponsesAPIPath(path string) bool {
	const prefix = "/api/provider/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return false
	}
	sub := rest[idx:]
	return sub == "/v1/responses" || sub == "/responses" ||
		strings.HasPrefix(sub, "/v1/responses?") || strings.HasPrefix(sub, "/responses?")
}
