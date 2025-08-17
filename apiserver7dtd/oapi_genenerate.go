// oapi_generate.go
package main

// 型だけ生成（internal/oapi/types.gen.go）
//go:generate oapi-codegen -generate types -o internal/oapi/types.gen.go -package oapi openapi.yaml
