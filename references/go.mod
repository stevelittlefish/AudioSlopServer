// Not a real module, and never built. Yes, a go.mod that exists purely to be
// ignored. Welcome to Go tooling.
//
// Several reference clones contain stray Go files, and without this they get
// absorbed into ASS's own module: `go vet ./...` walks them, `gofmt -l .`
// reports them, and `go test ./...` tries to build them. Delightful.
//
// A go.mod makes this directory the root of a separate module, and Go excludes
// such trees from the parent's ./... patterns. It is committed rather than
// created by pull.sh so a fresh checkout is protected before any clone exists.
module references

go 1.26
