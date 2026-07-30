FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY packages ./packages
COPY services/einvoicing ./services/einvoicing
RUN CGO_ENABLED=0 go build -o /bin/einvoicing ./services/einvoicing

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/einvoicing /bin/einvoicing
ENV PORT=8110 DATA_DIR=/data
EXPOSE 8110
ENTRYPOINT ["/bin/einvoicing"]
