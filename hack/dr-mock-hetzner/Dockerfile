FROM golang:1.24-alpine AS build
WORKDIR /app
COPY main.go go.mod .
RUN CGO_ENABLED=0 go build -o /mock .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /mock /mock
EXPOSE 8089
ENTRYPOINT ["/mock"]
