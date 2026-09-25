FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /broker .

FROM alpine:3.21
RUN adduser -D -u 10001 jump
USER jump
COPY --from=build /broker /broker
EXPOSE 8080
ENTRYPOINT ["/broker"]
