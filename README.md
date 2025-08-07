# redic

[![Go Version](https://img.shields.io/badge/go-%3E%3D1.16-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/jpzz/redic)](https://goreportcard.com/report/github.com/jpzz/redic)

一个基于redigo的客户端库，提供自动重连、智能订阅管理和连接状态监控等。

## ✨ 特性

- 🔄 **智能重连机制** - 自动检测网络故障并进行指数退避重连
- 📡 **订阅管理** - 支持频道订阅和模式订阅，断线后自动重新订阅
- 📊 **连接状态监控** - 实时监控连接状态和重连尝试
- 🎯 **事件回调** - 丰富的连接事件回调机制
- 🛡️ **错误处理** - 智能区分网络错误和业务错误
- 🚀 **高性能** - 基于连接池，支持并发操作
- 🔧 **易于使用** - 简洁的 API 设计，开箱即用

## 📦 安装

```bash
go get github.com/jpzz/redic
