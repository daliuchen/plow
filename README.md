# plow <!-- omit in toc -->

[![build](https://github.com/six-ddc/plow/actions/workflows/release.yml/badge.svg)](https://github.com/six-ddc/plow/actions/workflows/release.yml)
[![Homebrew](https://img.shields.io/badge/dynamic/json.svg?url=https://formulae.brew.sh/api/formula/plow.json&query=$.versions.stable&label=homebrew)](https://formulae.brew.sh/formula/plow)
[![GitHub license](https://img.shields.io/github/license/six-ddc/plow.svg)](https://github.com/six-ddc/plow/blob/main/LICENSE)
[![made-with-Go](https://img.shields.io/badge/Made%20with-Go-1f425f.svg)](http://golang.org)

Plow is an HTTP(S) benchmarking tool, written in Golang. It uses
excellent [fasthttp](https://github.com/valyala/fasthttp#http-client-comparison-with-nethttp) instead of Go's default
net/http due to its lightning fast performance.

Hi, I'm Liu Chen, and I have added some features to Plow:

1. Path parameters
2. Random query
3. Response status line chart

![demo1.gif](demo1.gif)
