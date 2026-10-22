FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go test ./... && CGO_ENABLED=0 go build -o /cursor-agent-mcp .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /cursor-agent-mcp /cursor-agent-mcp
EXPOSE 8080
ENTRYPOINT ["/cursor-agent-mcp"]
