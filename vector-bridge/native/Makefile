# 构建本地动态库
#   Linux : build/libvecidx.so
#   macOS : build/libvecidx.dylib
#   Windows (MinGW): build/vecidx.dll
CXX      ?= g++
CXXFLAGS ?= -std=c++17 -O2 -fPIC -Wall -Wextra -pthread
BUILD    := build

UNAME_S := $(shell uname -s 2>/dev/null || echo Windows)

ifeq ($(UNAME_S),Darwin)
  LIB := $(BUILD)/libvecidx.dylib
  LDFLAGS_LIB := -dynamiclib
else ifneq (,$(findstring MINGW,$(UNAME_S))$(findstring MSYS,$(UNAME_S))$(findstring CYGWIN,$(UNAME_S)))
  LIB := $(BUILD)/vecidx.dll
  LDFLAGS_LIB := -shared -Wl,--out-implib,$(BUILD)/libvecidx.a
else
  LIB := $(BUILD)/libvecidx.so
  LDFLAGS_LIB := -shared -Wl,-soname,libvecidx.so.1
endif

.PHONY: all clean
all: $(LIB)

$(LIB): vector_index.cpp vector_index.h | $(BUILD)
	$(CXX) $(CXXFLAGS) -DVI_BUILD_DLL $(LDFLAGS_LIB) -o $@ vector_index.cpp

$(BUILD):
	mkdir -p $(BUILD)

clean:
	rm -rf $(BUILD)
