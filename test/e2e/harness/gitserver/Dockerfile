FROM alpine:3.22
RUN apk add --no-cache git git-daemon nginx fcgiwrap spawn-fcgi \
    && test -x /usr/libexec/git-core/git-http-backend
COPY nginx.conf /etc/nginx/nginx.conf
EXPOSE 80
CMD ["/bin/sh", "-c", "spawn-fcgi -s /run/fcgiwrap.sock -M 0666 -- /usr/bin/fcgiwrap && exec nginx -g 'daemon off;'"]
