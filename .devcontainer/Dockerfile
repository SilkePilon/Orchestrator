FROM mcr.microsoft.com/devcontainers/base:ubuntu26.04

ENV DEBIAN_FRONTEND=noninteractive

# All GNOME/GTK4/libadwaita dependencies are available as native packages
# on Ubuntu 26.04 (Resolute): libadwaita 1.9.0, GLib 2.88.0, GTK4 4.18+
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    cmake gcc pkg-config \
    libglib2.0-dev \
    libgtk-4-dev \
    libadwaita-1-dev \
    libgtksourceview-5-dev \
    libgirepository-2.0-dev \
    libgraphene-1.0-dev \
    libfontconfig1-dev \
    libwayland-dev \
    libgles2-mesa-dev \
    libegl1-mesa-dev \
    libffi-dev \
    libxcursor-dev \
    libgl1-mesa-dev \
    libnotify-dev \
    libdbus-1-dev \
    libsass-dev \
    libgstreamer1.0-dev \
    libgstreamer-plugins-base1.0-dev \
    libgstreamer-plugins-bad1.0-dev \
    libappstream-dev \
    valac \
    flatpak-builder \
    dbus-x11 \
    glslc && \
    rm -rf /var/lib/apt/lists/*

# On Ubuntu 26.04 / GLib 2.80+, gobject-introspection-1.0.pc was replaced by
# girepository-2.0.pc and headers moved into glib-2.0/girepository/.
# gotk4 CGo bindings still reference the old pkg-config name and include path,
# so provide a compatibility shim.
# VTE 0.78+ ships vte-2.91-gtk4.pc. Ubuntu 26.04 does not yet package it,
# so build it from source.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        meson ninja-build gettext \
        libpcre2-dev libxml2-dev libgnutls28-dev libcairo2-dev \
        libpango1.0-dev libsystemd-dev libxcb1-dev python3-gi-docgen && \
    rm -rf /var/lib/apt/lists/* && \
    git clone --depth=1 --branch=0.78.0 \
        https://gitlab.gnome.org/GNOME/vte.git /tmp/vte && \
    meson setup /tmp/vte-build /tmp/vte \
        -Ddocs=false -Dvapi=false -Dsixel=false \
        -Dgtk3=false -Dgtk4=true && \
    ninja -C /tmp/vte-build install && \
    rm -rf /tmp/vte /tmp/vte-build

RUN mkdir -p /usr/include/gobject-introspection-1.0 && \
    ln -sf /usr/include/glib-2.0/girepository/girepository.h \
    /usr/include/gobject-introspection-1.0/girepository.h && \
    PCDIR=$(pkg-config --variable=pcfiledir girepository-2.0) && \
    printf '[Desktop Entry]\n' > /dev/null && \
    printf 'prefix=/usr\nincludedir=${prefix}/include\nlibdir=${prefix}/lib/x86_64-linux-gnu\n\nName: gobject-introspection-1.0\nDescription: GObject Introspection (compat shim for girepository-2.0)\nVersion: 2.88.0\nRequires: glib-2.0, gobject-2.0\nLibs: -L${libdir} -lgirepository-2.0\nCflags: -I${includedir}/glib-2.0/girepository\n' \
    > /usr/lib/x86_64-linux-gnu/pkgconfig/gobject-introspection-1.0.pc
        