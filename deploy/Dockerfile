FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dbtool ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dbtool /dbtool
# 端口默认 8000；要改端口：挂载配置文件并指定  -v ./dbtool.toml:/dbtool.toml  -e DB_TOOL_CONFIG=/dbtool.toml
# （见 configs/dbtool.example.toml），或直接设环境变量 LISTEN=:9000；同时改 EXPOSE / -p 映射。
EXPOSE 8000
ENTRYPOINT ["/dbtool"]
