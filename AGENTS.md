# Agent Capsule Coding Spec

- `capsule import` / `capsule restore` 的效果必须对齐 Codex 原生 session fork：导入后出现一条新的 thread，可见、可打开、可继续；源 thread 永远不被覆盖。即使 source 和 target 是同一个 `CODEX_HOME`，也应该生成新 thread。
