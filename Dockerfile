FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /carbon-api ./cmd

FROM docker.io/library/alpine:3.23
RUN apk --no-cache add ca-certificates tzdata
COPY --from=build /carbon-api /usr/local/bin/carbon-api
USER 1000
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/carbon-api"]
