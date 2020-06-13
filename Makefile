BUILDDIR=build
PLUGINBUILDDIR=$(BUILDDIR)/plugins
PLUGINBUILDER=./scripts/build-plugin.sh
BINARY=$(BUILDDIR)/rhp
GO=/usr/local/go/bin/go
GOOPTS=
SRCS=main.go types.go

all: clean $(BINARY) rpjios-plugin

debug: GOOPTS += -race
debug: all

rpjios-plugin:
	BUILD_PLUGIN_OUTDIR="$(PLUGINBUILDDIR)" GOOPTS=$(GOOPTS) $(PLUGINBUILDER) plugins/rpjios

$(BINARY): $(SRCS)
	$(GO) fmt
	$(GO) build -o $(BINARY) $(GOOPTS) $(SRCS)

run: debug rpjios-plugin
	./$(BINARY) -listen 0.0.0.0

.PHONY: clean
clean:
	rm -fr $(BUILDDIR)
	$(GO) clean
