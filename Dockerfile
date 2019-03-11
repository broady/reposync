FROM golang:1.12 as build
WORKDIR /src/app
COPY . .
RUN go build -v .

FROM debian:stretch-slim
RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/app/reposync /bin/reposync
CMD ["/bin/reposync"]
