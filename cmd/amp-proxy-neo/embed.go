package main

import "embed"

//go:embed webui-react/dist
var chatUIFiles embed.FS

//go:embed examples/*.yaml
var exampleConfigFiles embed.FS
