FROM progrium/busybox

COPY bin/battery-historian /data/battery-historian
COPY compiled /data/compiled
COPY templates /data/templates
COPY static /data/static

WORKDIR /data
ENTRYPOINT "./battery-historian"
EXPOSE 9999
