// Code to demonstrate https://bugs.chromium.org/p/chromium/issues/detail?id=735583#c1
//
// Serves HTTP/1 at http://issue735583.bradfitz.com/ (repros)
// and:
// Serves HTTP/2 at https://issue735583.bradfitz.com/ (doesn't repro)

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

var (
	httpListen  = flag.String("http", ":8080", "HTTP listen address")
	httpsListen = flag.String("https", ":4430", "HTTPS listen address")
)

func main() {
	flag.Parse()

	mux := new(http.ServeMux)
	mux.HandleFunc("/", handle6MJPEGRoot)
	mux.HandleFunc("/other-page", handle6MJPEGOtherPage)

	ln1, err := net.Listen("tcp", *httpListen)
	if err != nil {
		log.Fatal(err)
	}
	ln2, err := net.Listen("tcp", *httpsListen)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Running HTTP port at %v, HTTPS port at %v.", *httpListen, *httpsListen)

	errc := make(chan error, 2)
	go func() {
		err := http.Serve(ln1, mux)
		err = fmt.Errorf("Failure running HTTP server: %w", err)
		errc <- err
	}()

	srv := httptest.NewUnstartedServer(mux)
	srv.Listener.Close()
	srv.Listener = ln2
	srv.EnableHTTP2 = true
	srv.StartTLS()

	log.Fatal(<-errc)
}

func handle6MJPEGRoot(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.RequestURI, ".mjpg") {
		handle6MJPEGStream(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	var msg string
	if r.TLS == nil || r.ProtoMajor == 1 {
		msg = fmt.Sprintf("You're using %v; you <b>should</b> see the bug. You will be unable to click link below.", r.Proto)
	} else {
		msg = fmt.Sprintf("You're using %v; you <b>should NOT</b> see the bug repro.", r.Proto)
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "<html><body>\n<h1>Issue 735583 Demo</h1>\n<p>For background, see <a href='https://bugs.chromium.org/p/chromium/issues/detail?id=735583'>Chrome Issue 735583</a>.</p>\n")
	fmt.Fprintf(&buf, "<p>This page by defeault streams 6 MJPEG streams (change with URL param <code>?n=</code>) in &lt;img&gt; tags to demonstrate that over HTTP/1.1, navigation to another page on the site via an &lt;a&gt; link is busted.\nIt works with HTTP/2 and fails with plaintext HTTP/1.x.</p>\n")
	fmt.Fprintf(&buf, "<p>%s</p>\n", msg)
	fmt.Fprintf(&buf, "\n<h2>\n<a href='/other-page'>Some same-host link to another page</a> &lt-- click me if you can</h2>\n")

	var n int
	if r.Method == "GET" {
		n, _ = strconv.Atoi(r.FormValue("n"))
	}
	if n < 1 {
		n = 6
	}
	if n > 10 {
		n = 10
	}

	buf.WriteString("\n<p>\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&buf, "<img src='/cam%d/%d/stream.mjpg' width=100 height=100 style='border: 2px solid black'>\n", i, time.Now().UnixNano())
	}
	buf.WriteString("</body></html>\n")
	w.Write(buf.Bytes())
}

func handle6MJPEGOtherPage(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "<html><body>Some other page on the site.")
}

func handle6MJPEGStream(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fetchReq := make(chan bool, 1)
	jpegc := make(chan interface{}, 1) // of error or []byte
	go func() {
		for {
			select {
			case <-fetchReq:
				jpeg, err := getRandomJPEG(ctx)
				if err != nil {
					jpegc <- err
				} else {
					jpegc <- jpeg
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	mw := multipart.NewWriter(rw)
	rw.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+mw.Boundary())
	rw.Header().Set("Cache-Control", "no-cache")
	frame := 0
	for {
		t0 := time.Now()
		fetchReq <- true

		select {
		case <-ctx.Done():
			return
		case res := <-jpegc:
			if err, ok := res.(error); ok {
				log.Printf("error getting issue735583 jpeg: %v", err)
				return
			}
			frame++
			jpegBytes := res.([]byte)
			sendCount := 1
			if frame == 1 {
				sendCount = 2 // browsers suck
			}
			for i := 0; i < sendCount; i++ {
				w, err := mw.CreatePart(textproto.MIMEHeader{
					"Content-Type":   {"image/jpeg"},
					"Content-Length": {fmt.Sprint(len(jpegBytes))},
				})
				if err != nil {
					return
				}
				if _, err := w.Write(jpegBytes); err != nil {
					return
				}
				rw.(http.Flusher).Flush()
			}
		}

		delay := 500 * time.Millisecond
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(t0.Add(delay))):
		}
	}
}

func getRandomJPEG(ctx context.Context) ([]byte, error) {
	im := image.NewRGBA(image.Rect(0, 0, 25, 25))
	r, g, b := byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256))
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i] = r
	}
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i+1] = g
	}
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i+2] = b
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, nil); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
