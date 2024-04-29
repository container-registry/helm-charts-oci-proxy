package registry

import "net/http"

type Handler func(resp http.ResponseWriter, req *http.Request) error
