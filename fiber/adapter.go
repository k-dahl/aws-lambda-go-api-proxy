// Package fiberadapter adds Fiber support for the aws-severless-go-api library.
// Uses the core package behind the scenes and exposes the New method to
// get a new instance and Proxy method to send request to the Fiber app.
package fiberadapter

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/valyala/fasthttp"

	"github.com/awslabs/aws-lambda-go-api-proxy/core"
)

// FiberLambda makes it easy to send API Gateway proxy events to a fiber.App.
// The library transforms the proxy event into an HTTP request and then
// creates a proxy response object from the *fiber.Ctx
type FiberLambda struct {
	core.RequestAccessor
	v2  core.RequestAccessorV2
	app *fiber.App
}

// New creates a new instance of the FiberLambda object.
// Receives an initialized *fiber.App object - normally created with fiber.New().
// It returns the initialized instance of the FiberLambda object.
func New(app *fiber.App) *FiberLambda {
	return &FiberLambda{
		app: app,
	}
}

// Proxy receives an API Gateway proxy event, transforms it into an http.Request
// object, and sends it to the fiber.App for routing.
// It returns a proxy response object generated from the http.ResponseWriter.
func (f *FiberLambda) Proxy(req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	fiberRequest, err := f.ProxyEventToHTTPRequest(req)
	return f.proxyInternal(fiberRequest, err)
}

// ProxyV2 is just same as Proxy() but for APIGateway HTTP payload v2
func (f *FiberLambda) ProxyV2(req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	fiberRequest, err := f.v2.ProxyEventToHTTPRequest(req)
	return f.proxyInternalV2(fiberRequest, err)
}

// ProxyWithContext receives context and an API Gateway proxy event,
// transforms them into an http.Request object, and sends it to the echo.Echo for routing.
// It returns a proxy response object generated from the http.ResponseWriter.
func (f *FiberLambda) ProxyWithContext(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	fiberRequest, err := f.EventToRequestWithContext(ctx, req)
	return f.proxyInternal(fiberRequest, err)
}

// ProxyWithContextV2 is just same as ProxyWithContext() but for APIGateway HTTP payload v2
func (f *FiberLambda) ProxyWithContextV2(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	fiberRequest, err := f.v2.EventToRequestWithContext(ctx, req)
	return f.proxyInternalV2(fiberRequest, err)
}

func (f *FiberLambda) proxyInternal(req *http.Request, err error) (events.APIGatewayProxyResponse, error) {

	if err != nil {
		return core.GatewayTimeout(), core.NewLoggedError("Could not convert proxy event to request: %v", err)
	}

	resp := core.NewProxyResponseWriter()
	f.adaptor(resp, req)

	proxyResponse, err := resp.GetProxyResponse()
	if err != nil {
		return core.GatewayTimeout(), core.NewLoggedError("Error while generating proxy response: %v", err)
	}

	return proxyResponse, nil
}

func (f *FiberLambda) proxyInternalV2(req *http.Request, err error) (events.APIGatewayV2HTTPResponse, error) {

	if err != nil {
		return core.GatewayTimeoutV2(), core.NewLoggedError("Could not convert proxy event to request: %v", err)
	}

	resp := core.NewProxyResponseWriterV2()
	f.adaptor(resp, req)

	proxyResponse, err := resp.GetProxyResponse()
	if err != nil {
		return core.GatewayTimeoutV2(), core.NewLoggedError("Error while generating proxy response: %v", err)
	}

	return proxyResponse, nil
}

type IpInfo struct {
	IsIPv4  bool
	IsIPv6  bool
	HasPort bool
}

func IsIPv4(address string) bool {
	return strings.Count(address, ":") < 2
}

func IsIPv6(address string) bool {
	return strings.Count(address, ":") >= 2
}

func IpHasPort(address string) bool {
	return strings.HasSuffix(address, ":80")
}

func GetIpInfo(address string) IpInfo {
	return IpInfo{
		IsIPv4:  IsIPv4(address),
		IsIPv6:  IsIPv6(address),
		HasPort: IpHasPort(address),
	}
}

func (f *FiberLambda) adaptor(w http.ResponseWriter, r *http.Request) {
	// New fasthttp request
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	// Convert net/http -> fasthttp request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, utils.StatusMessage(fiber.StatusInternalServerError), fiber.StatusInternalServerError)
		return
	}
	req.Header.SetContentLength(len(body))
	_, _ = req.BodyWriter().Write(body)

	req.Header.SetMethod(r.Method)
	req.SetRequestURI(r.RequestURI)
	req.SetHost(r.Host)
	for key, val := range r.Header {
		for _, v := range val {
			switch key {
			case fiber.HeaderHost,
				fiber.HeaderContentType,
				fiber.HeaderUserAgent,
				fiber.HeaderContentLength,
				fiber.HeaderConnection:
				req.Header.Set(key, v)
			default:
				req.Header.Add(key, v)
			}
		}
	}

	// Get info about IP.
	ipInfo := GetIpInfo(r.RemoteAddr)

	var addrWithPort string

	if ipInfo.IsIPv4 && !ipInfo.HasPort {
		addrWithPort = r.RemoteAddr + ":80" // assuming a default port
	}

	if ipInfo.IsIPv6 && !ipInfo.HasPort {
		addrWithPort = fmt.Sprintf("[%s]%s", r.RemoteAddr, ":80")
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", addrWithPort)
	if err != nil {
		fmt.Printf("could not resolve TCP address for addr %s\n", r.RemoteAddr)
		log.Println(err)
		http.Error(w, utils.StatusMessage(fiber.StatusInternalServerError), fiber.StatusInternalServerError)
		return
	}

	// New fasthttp Ctx
	var fctx fasthttp.RequestCtx
	fctx.Init(req, remoteAddr, nil)

	// Pass RequestCtx to Fiber router
	f.app.Handler()(&fctx)

	// Set response headers
	fctx.Response.Header.VisitAll(func(k, v []byte) {
		w.Header().Add(utils.UnsafeString(k), utils.UnsafeString(v))
	})

	// Set response statuscode
	w.WriteHeader(fctx.Response.StatusCode())

	// Set response body
	_, _ = w.Write(fctx.Response.Body())
}
