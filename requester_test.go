package main

import (
	"github.com/stretchr/testify/assert"
	"github.com/valyala/fasthttp"
	"testing"
)

func Test_newRequestUrlBuilder(t *testing.T) {
	opt := new(ClientOpt)
	t.Run("success when no path param", func(t *testing.T) {
		opt.url = "https://www.jinshuju.com/pt/afderf"
		builder, err := newRequestUrlBuilder(opt)
		assert.Nil(t, err)
		assert.Empty(t, builder.pathParams)
	})
	t.Run("success when match path param", func(t *testing.T) {
		opt.url = "https://www.jinshuju.com/pt/:token/:widget"
		opt.pathParams = []string{
			"token:adfefd",
			"widget:[fdsdf,efgr,fewef]",
		}
		builder, err := newRequestUrlBuilder(opt)
		assert.Nil(t, err)
		expectValue := map[string][]string{
			"token":  {"adfefd"},
			"widget": {"fdsdf", "efgr", "fewef"},
		}
		assert.EqualValues(t, builder.pathParams, expectValue)
	})
	t.Run("fail when path params not match", func(t *testing.T) {
		opt.url = "https://www.jinshuju.com/pt/:token/:widget:/:back"
		opt.pathParams = []string{
			"token:adfefd",
			"widget:[fdsdf,efgr,fewef]",
		}
		builder, err := newRequestUrlBuilder(opt)
		assert.NotNil(t, err)
		assert.Nil(t, builder)
	})
}

func Test_requestUrlBuilder(t *testing.T) {
	t.Run("success when need use random query", func(t *testing.T) {
		rawUri := "http://www.jinshuju.com/pt/fdfefe"

		opt := new(ClientOpt)
		opt.useRandomQuery = true
		opt.url = rawUri
		builder, err := newRequestUrlBuilder(opt)
		assert.Nil(t, err)
		requestUri := new(fasthttp.URI)
		err = requestUri.Parse([]byte("jinshuju.com"), []byte(rawUri))
		assert.Nil(t, err)

		builder.Uri(requestUri)
		args := requestUri.QueryArgs()
		assert.Equal(t, 1, args.Len())
	})

	t.Run("success when path have query and  need use random query", func(t *testing.T) {
		rawUri := "http://www.jinshuju.com/pt/fdfefe?query1=1"

		opt := new(ClientOpt)
		opt.useRandomQuery = true
		opt.url = rawUri
		builder, err := newRequestUrlBuilder(opt)
		assert.Nil(t, err)
		requestUri := new(fasthttp.URI)
		err = requestUri.Parse([]byte("jinshuju.com"), []byte(rawUri))
		assert.Nil(t, err)

		builder.Uri(requestUri)
		args := requestUri.QueryArgs()
		assert.Equal(t, 2, args.Len())
	})

	t.Run("success when path have param and need use random query", func(t *testing.T) {
		rawUri := "http://www.jinshuju.com/pt/:token/:widget?query1=1"

		opt := new(ClientOpt)
		opt.useRandomQuery = true
		opt.url = rawUri
		opt.pathParams = []string{
			"token:[a,c,d,e]",
			"widget:[ff,c,fdfd,cdfe]",
		}
		builder, err := newRequestUrlBuilder(opt)
		assert.Nil(t, err)
		requestUri := new(fasthttp.URI)
		err = requestUri.Parse([]byte("jinshuju.com"), []byte(rawUri))
		assert.Nil(t, err)

		builder.Uri(requestUri)
		args := requestUri.QueryArgs()
		assert.Equal(t, 2, args.Len())
		print(string(requestUri.RequestURI()))
	})
}
func Benchmark_requestUrlBuilder(b *testing.B) {
	for i := 0; i < b.N; i++ {
		rawUri := "http://www.jinshuju.com/pt/:token/:widget?query1=1"

		opt := new(ClientOpt)
		opt.useRandomQuery = true
		opt.url = rawUri
		opt.pathParams = []string{
			"token:[a,c,d,e]",
			"widget:[ff,c,fdfd,cdfe]",
		}
		builder, err := newRequestUrlBuilder(opt)
		assert.Nil(b, err)
		requestUri := new(fasthttp.URI)
		err = requestUri.Parse([]byte("jinshuju.com"), []byte(rawUri))
		assert.Nil(b, err)

		builder.Uri(requestUri)
		args := requestUri.QueryArgs()
		assert.Equal(b, 2, args.Len())
		println(string(requestUri.RequestURI()))
	}
}
