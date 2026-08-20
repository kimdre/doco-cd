FROM nginx:1.29-alpine
RUN apk add --no-cache git git-daemon fcgiwrap spawn-fcgi \
    && test -x /usr/libexec/git-core/git-http-backend \
    # bind-mounted repos are owned by the host user (CI: uid 1001), while
    # git-http-backend runs as root - without this git refuses them with
    # "dubious ownership" and the server answers 500
    && git config --system safe.directory '*'
COPY nginx.conf /etc/nginx/nginx.conf
EXPOSE 80
CMD ["/bin/sh", "-c", "spawn-fcgi -s /run/fcgiwrap.sock -M 0666 -- /usr/bin/fcgiwrap && exec nginx -g 'daemon off;'"]
