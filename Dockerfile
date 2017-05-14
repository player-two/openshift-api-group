FROM golang:1.8.1

WORKDIR $GOPATH/src/github.com/megalord/openshift-api-group/
COPY glide.yaml glide.lock ./
RUN go get -u github.com/Masterminds/glide && \
    glide install --strip-vendor

COPY . .
RUN go install

EXPOSE 8080
CMD ["openshift-api-group"]
