ARG GOVERSION=golang:1.21.0
FROM $GOVERSION
WORKDIR /app
ADD . /app/
RUN go build -o /app/main .
CMD ["/app/main"]
