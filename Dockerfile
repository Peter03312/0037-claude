# 纯 Go 1.24 标准库构建，无外部依赖、无前端、无数据库。
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -o /out/smoke ./cmd/smoke

FROM alpine:3.20
RUN adduser -D -u 10001 appuser
WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build /out/smoke /app/smoke
USER appuser
EXPOSE 8080
ENTRYPOINT ["/app/api"]
