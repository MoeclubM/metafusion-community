# MetaFusion Community

MetaFusion 社区交流、讨论版块、楼层回复与条目动态评分系统微服务。

## 💬 核心定位

作为 MetaFusion 平台的独立社区讨论中枢，负责话题交流、条目讨论楼、评分打分与用户互动。通过单向只读引用实体 UUID（`target_entity_id`）挂载到元数据系统，不侵入核心目录数据库。

- **主项目 (Core Catalog)**: [MetaFusion](https://github.com/MoeclubM/MetaFusion)

## 🚀 启动运行

```bash
go run cmd/server/main.go
```
