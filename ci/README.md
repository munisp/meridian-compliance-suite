# CI workflow copy

`ci/workflows/ci.yml` is a byte-identical copy of `.github/workflows/ci.yml`
(HARDENING.md H6). If the push token lacks the `workflow` scope, GitHub rejects
pushes that touch `.github/workflows/`; in that case this copy is the source of
truth — move it manually:

    mkdir -p .github/workflows
    cp ci/workflows/ci.yml .github/workflows/ci.yml

Pipeline: per Go module `go build ./... && go vet ./... && go test -race ./...`
(root module + case-mgmt, pos-vat, vasp-carf), `pytest` for services/etr, and
`npm ci && npm run build` for portals/compliance-portal.
