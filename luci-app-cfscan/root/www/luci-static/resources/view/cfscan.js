'use strict';
'require view';

/*
 * luci-app-cfscan 前端：把 cfscan 容器设置页（默认 7788 端口）内嵌到 LuCI 菜单里。
 * 容器部署见项目仓库 docker-compose.yml（端口映射 7788:8080）。
 */
var CFSCAN_PORT = '7788';

return view.extend({
	render: function () {
		var url = 'http://' + window.location.hostname + ':' + CFSCAN_PORT + '/';
		return E('div', {}, [
			E('iframe', {
				src: url,
				style: 'width: 100%; height: calc(100vh - 210px); min-height: 560px; border: none; border-radius: 3px; background: #fff;'
			}),
			E('p', { 'class': 'cbi-section-descr' }, [
				_('如果上方页面空白，说明 cfscan 容器还没启动：请到 Docker 里启动 cfscan 容器，或新开页面访问 '),
				E('a', { href: url, target: '_blank' }, url)
			])
		]);
	},
	handleSaveApply: null,
	handleSave: null,
	handleReset: null
});
