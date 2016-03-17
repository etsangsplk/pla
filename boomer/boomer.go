// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package boomer provides commands to run load tests and display results.
package boomer

import (
	"crypto/tls"
	"github.com/valyala/fasthttp"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/sschepens/pb"
)

var client *fasthttp.Client

type result struct {
	err           error
	statusCode    int
	duration      time.Duration
	contentLength int
}

type Boomer struct {
	// Request is the request to be made.
	Request *fasthttp.Request

	// N is the total number of requests to make.
	N int

	// C is the concurrency level, the number of concurrent workers to run.
	C int

	// Duration is the amount of time the test should run.
	Duration time.Duration

	// Timeout in seconds.
	Timeout time.Duration

	// QPS is the rate limit.
	QPS int

	// Output represents the output type. If "csv" is provided, the
	// output will be dumped as a csv stream.
	Output string

	// ProxyAddr is the address of HTTP proxy server in the format on "host:port".
	// Optional.
	ProxyAddr *url.URL

	// ReadAll determines whether the body of the response needs
	// to be fully consumed.
	ReadAll bool

	bar     *pb.ProgressBar
	results chan *result
	stop    chan struct{}
}

func (b *Boomer) startProgress() {
	if b.Output != "" {
		return
	}
	total := b.N
	if b.Duration > 0 {
		total = 100
	}
	b.bar = pb.New(total)
	b.bar.Format("Bom !")
	b.bar.BarStart = "Pl"
	b.bar.BarEnd = "!"
	b.bar.Empty = " "
	b.bar.Current = "a"
	b.bar.CurrentN = "a"
	b.bar.Start()
}

func (b *Boomer) finalizeProgress() {
	if b.Output != "" {
		return
	}
	b.bar.Finish()
}

func (b *Boomer) incProgress() {
	if b.Output != "" {
		return
	}
	b.bar.Increment()
}

func (b *Boomer) stopProgress() *time.Timer {
		shutdownTimer := time.AfterFunc(10*time.Second, func() {
			b.finalizeProgress()
			close(b.stop)
			os.Exit(1)
		})
		b.finalizeProgress()
		close(b.stop)
		return shutdownTimer
}

// Run makes all the requests, prints the summary. It blocks until
// all work is done.
func (b *Boomer) Run() {
	var shutdownTimer *time.Timer
	b.results = make(chan *result, b.C)
	b.stop = make(chan struct{})
	b.startProgress()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		<-c
		shutdownTimer = b.stopProgress()
	}()

	r := newReport(b.N, b.results, b.Output)
	if b.Duration > 0 {
		ticker := time.NewTicker(b.Duration / 100)
		go func () {
			for range ticker.C {
				b.incProgress()
			}
		}()
		time.AfterFunc(b.Duration, func() {
			shutdownTimer = b.stopProgress()
		})
	}
	b.runWorkers()
	if shutdownTimer != nil {
		shutdownTimer.Stop()
	}
	close(b.results)
	b.finalizeProgress()
	r.finalize()
}

func (b *Boomer) runWorker(wg *sync.WaitGroup, ch chan struct{}) {
	resp := fasthttp.AcquireResponse()
	req := fasthttp.AcquireRequest()
	b.Request.CopyTo(req)
	for range ch {
		s := time.Now()

		var code int
		var size int

		resp.Reset()
		var err error
		if b.Timeout > 0 {
			err = client.DoTimeout(req, resp, b.Timeout)
		} else {
			err = client.Do(req, resp)
		}
		if err == nil {
			size = resp.Header.ContentLength()
			code = resp.Header.StatusCode()
		}

		if b.ReadAll {
			resp.Body()
		}

    if b.Duration == 0 {
			b.incProgress()
		}
		b.results <- &result{
			statusCode:    code,
			duration:      time.Now().Sub(s),
			err:           err,
			contentLength: size,
		}
	}
	fasthttp.ReleaseResponse(resp)
	fasthttp.ReleaseRequest(req)
	wg.Done()
}

func (b *Boomer) runWorkers() {
	client = &fasthttp.Client{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		MaxConnsPerHost: b.C * 2,
	}
	var wg sync.WaitGroup
	wg.Add(b.C)

	var throttle <-chan time.Time
	if b.QPS > 0 {
		throttle = time.Tick(time.Duration(1e6/(b.QPS)) * time.Microsecond)
	}

	jobsch := make(chan struct{}, b.C)
	for i := 0; i < b.C; i++ {
		go b.runWorker(&wg, jobsch)
	}

	i := 0
Loop:
	for {
		if i >= b.N {
			break Loop
		}
		if b.QPS > 0 {
			<-throttle
		}
		select {
		case <-b.stop:
			break Loop
		case jobsch <- struct{}{}:
			if b.Duration == 0 {
				i++
			}
			continue
		}
	}
	close(jobsch)
	wg.Wait()
}
