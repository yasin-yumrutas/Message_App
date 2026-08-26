FROM golang:1.24-alpine AS build
WORKDIR /src
COPY Message_App/go.mod Message_App/go.sum ./
RUN go mod download
COPY Message_App/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relay .

FROM alpine:3.22
RUN addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/relay ./relay
COPY --from=build /src/index.html ./index.html
USER app
EXPOSE 8080
CMD ["./relay"]
