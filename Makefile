include $(TOPDIR)/rules.mk

PKG_NAME:=beszel-agent
PKG_VERSION:=0.9.1
PKG_RELEASE:=1

PKG_SOURCE:=beszel-$(PKG_VERSION).tar.gz
PKG_SOURCE_URL:=https://codeload.github.com/henrygd/beszel/tar.gz/v$(PKG_VERSION)?
PKG_HASH:=1d95096db7d4bf09c7164b4c7be5a73e35aabebec110795c643b2f565b939e7f

PKG_MAINTAINER:=Jackie264 <OneNAS-space>
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_USE_MIPS16:=0

GO_PKG:=github.com/henrygd/beszel/agent

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-values.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-plugins.mk

define Package/beszel-agent
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Beszel monitoring agent
  URL:=https://beszel.dev
  DEPENDS:=$(GO_ARCH_DEPENDS)
endef

define Package/beszel-agent/description
  Beszel is a lightweight server monitoring hub. 
  This package contains the agent that runs on the monitored system.
endef

define Package/beszel-agent/conffiles
/etc/config/beszel-agent
endef

define Build/Compile
	$(call GoPackage/Build/Compile/Default, \
		GoPKG="$(GO_PKG)" \
		GoBinPackage="0" \
	)
endef

define Package/beszel-agent/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/agent $(1)/usr/bin/beszel-agent

	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/beszel-agent.config $(1)/etc/config/beszel-agent

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/beszel-agent.init $(1)/etc/init.d/beszel-agent
endef

$(eval $(call GoPackage,beszel-agent))
$(eval $(call BuildPackage,beszel-agent))
