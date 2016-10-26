FROM progrium/busybox

RUN opkg-install python-light python-codecs

COPY bin/battery-historian /data/battery-historian
COPY compiled /data/compiled
COPY templates /data/templates
COPY static /data/static
COPY scripts /data/scripts
COPY third_party/flot-axislabels /data/third_party/flot-axislabels

WORKDIR /data
ENTRYPOINT "./battery-historian"
EXPOSE 9999
