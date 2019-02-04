##Test-gen 
The utility generates test stubs from the interface.
```go
//go:generate test-gen TestClient github.com/test/test.go
package test

type Client interface {
	Get() (string, error)
	Set(v string) error
}
``` 