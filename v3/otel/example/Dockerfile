FROM golang:alpine AS base
COPY . /src/
WORKDIR /src/instrumentation/github.com/gofiber/fiber/otelefiber/example

FROM base AS fiber-server
RUN go install ./server.go
CMD ["/go/bin/server"]
