// Package extauthz implements an Envoy ext_authz gRPC unary listener.
// Used by waypoint mode where both inbound validation and outbound exchange
// happen in a single Check() call.
package extauthz

import (
	"context"
	"net/http"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"

	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// Server implements the Envoy ext_authz Authorization gRPC service.
//
// InboundPipeline / OutboundPipeline are holders so the bound pipeline
// can be hot-swapped under the running listener; every Check() Loads
// through the holder, so in-flight requests finish on the pipeline they
// started with.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	InboundPipeline  *pipeline.Holder
	OutboundPipeline *pipeline.Holder
}

// Check handles a single ext_authz authorization request.
func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	httpReq := req.GetAttributes().GetRequest().GetHttp()
	if httpReq == nil {
		return deniedFromAction(codes.InvalidArgument,
			pipeline.DenyStatus(400, "auth.invalid-request", "missing HTTP request attributes")), nil
	}

	headers := httpReq.GetHeaders()
	host := headers[":authority"]
	if host == "" {
		host = headers["host"]
	}
	path := httpReq.GetPath()
	scheme := httpReq.GetScheme()

	// Inbound validation via pipeline
	inPctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Scheme:    scheme,
		Host:      host,
		Path:      path,
		Headers:   mapToHTTPHeader(headers),
		StartedAt: time.Now(),
	}
	// Finisher dispatch for inbound. Deferred before Run so the hook
	// fires whether the pipeline allows, denies, or Check returns
	// early.
	defer func() {
		s.InboundPipeline.RunFinish(ctx, inPctx, authzOutcome(inPctx))
	}()
	inAction := s.InboundPipeline.Run(ctx, inPctx)
	if inAction.Type == pipeline.Reject {
		return deniedFromAction(codes.Unauthenticated, inAction), nil
	}

	// Outbound exchange via pipeline
	outPctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    scheme,
		Host:      host,
		Path:      path,
		Headers:   mapToHTTPHeader(headers),
		StartedAt: time.Now(),
	}
	// Finisher dispatch for outbound. Only created/deferred if inbound
	// allowed — mirrors the two-pipeline control flow. Registered
	// AFTER the inbound defer so under LIFO this outbound finish runs
	// first, then inbound.
	defer func() {
		s.OutboundPipeline.RunFinish(ctx, outPctx, authzOutcome(outPctx))
	}()
	originalAuth := outPctx.Headers.Get("Authorization")
	outAction := s.OutboundPipeline.Run(ctx, outPctx)
	if outAction.Type == pipeline.Reject {
		return deniedFromAction(codes.PermissionDenied, outAction), nil
	}

	newAuth := outPctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return allowedWithToken(extractBearer(newAuth)), nil
	}
	return allowed(), nil
}

// authzOutcome builds a Finisher Outcome from pctx state. ext_authz is
// a check-only protocol with no HTTP status concept, so
// pipeline.OutcomeFromContext (which classifies StatusCode == 0 as
// OutcomeError) doesn't fit — the absence of a status here means
// "Check returned OK," not "error." We rely on
// pctx.RejectingPlugin(), the framework-stamped deny source, to
// distinguish allow from deny.
func authzOutcome(pctx *pipeline.Context) pipeline.Outcome {
	if denier := pctx.RejectingPlugin(); denier != "" {
		return pipeline.Outcome{
			FinalAction:   pipeline.OutcomeDeny,
			DenyingPlugin: denier,
		}
	}
	return pipeline.Outcome{FinalAction: pipeline.OutcomeAllow}
}

func mapToHTTPHeader(m map[string]string) http.Header {
	h := make(http.Header)
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func extractBearer(authHeader string) string {
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}

// deniedFromAction renders a pipeline Reject as an ext_authz CheckResponse
// preserving the plugin's status, headers, and body. The flat
// {"error":reason} body of the old denied() is gone — plugins can now
// supply structured bodies, Content-Type overrides, WWW-Authenticate
// challenges, Retry-After, etc., via action.Violation.
func deniedFromAction(code codes.Code, action pipeline.Action) *authv3.CheckResponse {
	status, headers, body := action.Violation.Render()
	setHeaders := make([]*corev3.HeaderValueOption, 0, len(headers))
	for k, vs := range headers {
		for _, v := range vs {
			setHeaders = append(setHeaders, &corev3.HeaderValueOption{
				Header: &corev3.HeaderValue{Key: k, Value: v},
			})
		}
	}
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{
			Code:    int32(code),
			Message: action.Violation.Reason,
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status:  &typev3.HttpStatus{Code: typev3.StatusCode(status)},
				Body:    string(body),
				Headers: setHeaders,
			},
		},
	}
}

func allowed() *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status:       &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
	}
}

func allowedWithToken(token string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				Headers: []*corev3.HeaderValueOption{
					{
						Header: &corev3.HeaderValue{
							Key:   "authorization",
							Value: "Bearer " + token,
						},
					},
				},
			},
		},
	}
}
