FROM syncthing/syncthing:1.14.0 AS syncthing
FROM okteto/remote:0.4.1 AS remote
FROM okteto/supervisor:0.1.4 AS supervisor
FROM okteto/clean:0.1.6 AS clean

FROM busybox

COPY --from=remote /usr/local/bin/remote /usr/local/bin/okteto-remote
COPY --from=supervisor /usr/local/bin/supervisor /usr/local/bin/okteto-supervisor
COPY --from=syncthing /bin/syncthing /usr/local/bin/syncthing
COPY --from=clean /usr/local/bin/clean /usr/local/bin/clean

# copy start
COPY start.sh /usr/local/bin/start.sh

ARG OKTETO_BIN_TAG
RUN echo okteto/bin:$OKTETO_BIN_TAG > /usr/local/bin/version.txt