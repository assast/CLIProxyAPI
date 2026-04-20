路由策略 加个 轮询-相关会话填充
  - OpenAI Responses：支持
  - Codex websocket：支持
  - Chat Completions：暂时不纳入这次会话粘性和透明代理范围
› 路由策略 加个 轮询-相关会话填充   新对话 轮询分配，同一个会话 使用填充算法   针对本项目 OpenAI / Codex是否可以做到


会话粘性 原先的报错回退逻辑还是需要，现在会话好像一直粘连在报错里面了
参考原本的填充策略的逻辑 移植过来
当软回退真的选到了新的 auth 时，明确记录“已重新绑定到哪个 auth”

这个打包的docker是否可以使用traefik管理

对比 git的
c4459c43462c45bcd8ac346c72b98647675a014e
c7508b7e8716037b45e72bbda35ebf9fa9cb3bf1 
2个版本，对比差异，生成本项目专用skill