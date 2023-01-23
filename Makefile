#
#  SPDX-License-Identifier: AGPL-3.0-only
#  Copyright (c) since 2022, mzz2017 (mzz@tuta.io). All rights reserved.
#

# The development version of clang is distributed as the 'clang' binary,
# while stable/released versions have a version number attached.
# Pin the default clang to a stable version.
CLANG ?= clang
STRIP ?= llvm-strip
CFLAGS := -O2 -g -Wall -Werror $(CFLAGS)

.PHONY: generate

# $BPF_CLANG is used in go:generate invocations.
generate: export BPF_CLANG := $(CLANG)
generate: export BPF_STRIP := $(STRIP)
generate: export BPF_CFLAGS := $(CFLAGS)
generate:
	go generate ./component/control/...