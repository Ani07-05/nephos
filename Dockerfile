FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /nephos .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 nephos \
 && mkdir /data && chown nephos /data
USER nephos
WORKDIR /data
COPY --from=build /nephos /usr/local/bin/nephos
EXPOSE 8080
ENTRYPOINT ["nephos"]
CMD ["serve", "-addr", ":8080", "-data", "/data"]
