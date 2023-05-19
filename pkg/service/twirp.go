/*
 * Copyright 2022 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package service

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/twitchtv/twirp"

	"github.com/whoyao/livekit/pkg/telemetry/prometheus"
	"github.com/whoyao/protocol/logger"
)

var (
	loggerKey         = struct{}{}
	statusReporterKey = struct{ a int }{42}
)

type twirpRequestFields struct {
	service string
	method  string
	error   twirp.Error
}

// logging handling inspired by https://github.com/bakins/twirpzap
// License: Apache-2.0
func TwirpLogger(logger logger.Logger) *twirp.ServerHooks {
	loggerPool := &sync.Pool{
		New: func() interface{} {
			return &requestLogger{
				logger:     logger,
				fieldsOrig: make([]interface{}, 0, 30),
			}
		},
	}
	return &twirp.ServerHooks{
		RequestReceived: func(ctx context.Context) (context.Context, error) {
			return requestReceived(ctx, loggerPool)
		},
		RequestRouted: responseRouted,
		Error:         errorReceived,
		ResponseSent: func(ctx context.Context) {
			responseSent(ctx, loggerPool)
		},
	}
}

type requestLogger struct {
	twirpRequestFields

	logger     logger.Logger
	fieldsOrig []interface{}
	fields     []interface{}
	startedAt  time.Time
}

func AppendLogFields(ctx context.Context, fields ...interface{}) {
	r, ok := ctx.Value(loggerKey).(*requestLogger)
	if !ok || r == nil {
		return
	}

	r.fields = append(r.fields, fields...)
}

func requestReceived(ctx context.Context, requestLoggerPool *sync.Pool) (context.Context, error) {
	r := requestLoggerPool.Get().(*requestLogger)
	r.startedAt = time.Now()
	r.fields = r.fieldsOrig
	r.error = nil

	if svc, ok := twirp.ServiceName(ctx); ok {
		r.service = svc
		r.fields = append(r.fields, "service", svc)
	}

	ctx = context.WithValue(ctx, loggerKey, r)
	return ctx, nil
}

func responseRouted(ctx context.Context) (context.Context, error) {
	if meth, ok := twirp.MethodName(ctx); ok {
		l, ok := ctx.Value(loggerKey).(*requestLogger)
		if !ok || l == nil {
			return ctx, nil
		}
		l.method = meth
		l.fields = append(l.fields, "method", meth)
	}

	return ctx, nil
}

func responseSent(ctx context.Context, requestLoggerPool *sync.Pool) {
	r, ok := ctx.Value(loggerKey).(*requestLogger)
	if !ok || r == nil {
		return
	}

	r.fields = append(r.fields, "duration", time.Since(r.startedAt))

	if status, ok := twirp.StatusCode(ctx); ok {
		r.fields = append(r.fields, "status", status)
	}
	if r.error != nil {
		r.fields = append(r.fields, "error", r.error.Msg())
		r.fields = append(r.fields, "code", r.error.Code())
	}

	serviceMethod := "API " + r.service + "." + r.method
	r.logger.Infow(serviceMethod, r.fields...)

	r.fields = r.fieldsOrig
	r.error = nil

	requestLoggerPool.Put(r)
}

func errorReceived(ctx context.Context, e twirp.Error) context.Context {
	r, ok := ctx.Value(loggerKey).(*requestLogger)
	if !ok || r == nil {
		return ctx
	}

	r.error = e

	return ctx
}

func TwirpRequestStatusReporter() *twirp.ServerHooks {
	return &twirp.ServerHooks{
		RequestReceived: statusReporterRequestReceived,
		RequestRouted:   statusReporterResponseRouted,
		Error:           statusReporterErrorReceived,
		ResponseSent:    statusReporterResponseSent,
	}
}

func statusReporterRequestReceived(ctx context.Context) (context.Context, error) {
	r := &twirpRequestFields{}

	if svc, ok := twirp.ServiceName(ctx); ok {
		r.service = svc
	}

	ctx = context.WithValue(ctx, statusReporterKey, r)
	return ctx, nil
}

func statusReporterResponseRouted(ctx context.Context) (context.Context, error) {
	if meth, ok := twirp.MethodName(ctx); ok {
		l, ok := ctx.Value(statusReporterKey).(*twirpRequestFields)
		if !ok || l == nil {
			return ctx, nil
		}
		l.method = meth
	}

	return ctx, nil
}

func statusReporterResponseSent(ctx context.Context) {
	r, ok := ctx.Value(statusReporterKey).(*twirpRequestFields)
	if !ok || r == nil {
		return
	}

	var statusFamily string
	if statusCode, ok := twirp.StatusCode(ctx); ok {
		if status, err := strconv.Atoi(statusCode); err == nil {
			switch {
			case status >= 400 && status <= 499:
				statusFamily = "4xx"
			case status >= 500 && status <= 599:
				statusFamily = "5xx"
			default:
				statusFamily = statusCode
			}
		}
	}

	var code twirp.ErrorCode
	if r.error != nil {
		code = r.error.Code()
	}

	prometheus.TwirpRequestStatusCounter.WithLabelValues(r.service, r.method, statusFamily, string(code)).Add(1)
}

func statusReporterErrorReceived(ctx context.Context, e twirp.Error) context.Context {
	r, ok := ctx.Value(statusReporterKey).(*twirpRequestFields)
	if !ok || r == nil {
		return ctx
	}

	r.error = e

	return ctx
}
