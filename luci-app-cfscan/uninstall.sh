#!/bin/sh
# luci-app-cfscan 卸载：删除菜单/ACL/视图文件并刷新 LuCI 缓存（不影响 Docker 容器本身）。
rm -f /usr/share/luci/menu.d/luci-app-cfscan.json
rm -f /usr/share/rpcd/acl.d/luci-app-cfscan.json
rm -f /www/luci-static/resources/view/cfscan.js
rm -rf /tmp/luci-modulecache /tmp/luci-indexcache
/etc/init.d/uhttpd restart || true
/etc/init.d/rpcd restart || true
echo "luci-app-cfscan 已卸载。"
