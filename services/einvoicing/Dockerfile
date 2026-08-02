FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages ./packages
COPY services/einvoicing ./services/einvoicing
COPY infra/postgres/migrations ./infra/postgres/migrations
RUN CGO_ENABLED=0 go build -o /bin/einvoicing ./services/einvoicing

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/einvoicing /bin/einvoicing
COPY --from=build /src/infra/postgres/migrations /migrations
ENV PORT=8110 DATA_DIR=/data MIGRATIONS_DIR=/migrations
EXPOSE 8110
ENTRYPOINT ["/bin/einvoicing"]
