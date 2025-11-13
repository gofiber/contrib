# SPNEGO Kerberos 认证中间件 for Fiber

[English Version](README.md)

该中间件为Fiber应用提供SPNEGO（简单受保护GSSAPI协商机制）认证，使HTTP请求能够使用Kerberos认证。

## 功能特点

- 通过SPNEGO机制实现Kerberos认证
- 灵活的keytab查找系统
- 支持从各种来源动态检索keytab
- 与Fiber上下文集成用于存储认证身份
- 可配置日志

## 版本兼容性

该中间件兼容：

- **Fiber v3**

## 安装

```bash
# 对于Fiber v3
go get github.com/gofiber/contrib/v3/spnego
```

## 使用方法

```go
package main

import (
	"fmt"
	"time"

	"github.com/gofiber/contrib/v3/spnego"
	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func main() {
	app := fiber.New()
	// 创建带有keytab查找函数的配置
	// 测试环境下，您可以使用utils.NewMockKeytab创建模拟keytab文件
	// 生产环境下，请使用真实的keytab文件
	_, clean, err := utils.NewMockKeytab(
		utils.WithPrincipal("HTTP/sso1.example.com"),
		utils.WithRealm("EXAMPLE.LOCAL"),
		utils.WithFilename("./temp-sso1.keytab"),
		utils.WithPairs(utils.EncryptTypePair{
			Version:     2,
			EncryptType: 18,
			CreateTime:  time.Now(),
		}),
	)
	if err != nil {
		log.Fatalf("创建模拟keytab失败: %v", err)
	}
	defer clean()
	keytabLookup, err := spnego.NewKeytabFileLookupFunc("./temp-sso1.keytab")
	if err != nil {
		log.Fatalf("创建keytab查找函数失败: %v", err)
	}
	
	// 创建中间件
	authMiddleware, err := spnego.NewSpnegoKrb5AuthenticateMiddleware(spnego.Config{
		KeytabLookup: keytabLookup,
	})
	if err != nil {
		log.Fatalf("创建中间件失败: %v", err)
	}

	// 将中间件应用于受保护的路由
	app.Use("/protected", authMiddleware)

	// 访问认证身份
	app.Get("/protected/resource", func(c fiber.Ctx) error {
		identity, ok := spnego.GetAuthenticatedIdentityFromContext(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).SendString("未授权")
		}
		return c.SendString(fmt.Sprintf("你好, %s!", identity.UserName()))
	})

	log.Info("服务器运行在 :3000")
	app.Listen(":3000")
}

## 动态Keytab查找

中间件的设计考虑了扩展性，允许从静态文件以外的各种来源检索keytab：

```go
// 示例：从数据库检索keytab
func dbKeytabLookup() (*keytab.Keytab, error) {
    // 您的数据库查找逻辑
    // ...
    return keytabFromDatabase, nil
}

// 示例：从远程服务检索keytab
func remoteKeytabLookup() (*keytab.Keytab, error) {
    // 您的远程服务调用逻辑
    // ...
    return keytabFromRemote, nil
}
```

## API参考

### `NewSpnegoKrb5AuthenticateMiddleware(cfg Config) (fiber.Handler, error)`

创建新的SPNEGO认证中间件。

### `GetAuthenticatedIdentityFromContext(ctx fiber.Ctx) (goidentity.Identity, bool)`

从Fiber上下文中检索已认证的身份。

### `NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error)`

创建一个新的KeytabLookupFunc，用于加载keytab文件。

## 配置

`Config`结构体支持以下字段：

- `KeytabLookup`：检索keytab的函数（必需）
- `Log`：用于中间件日志记录的日志记录器（可选，默认为Fiber的默认日志记录器）

## 要求

- Fiber v3
- Kerberos基础设施

## 注意事项

- 确保正确配置Kerberos基础架构
- 中间件处理SPNEGO协商过程
- 已认证的身份使用`contextKeyOfIdentity`存储在Fiber上下文中
```
