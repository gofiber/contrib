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

该中间件提供两个版本以支持不同的Fiber版本：

- **v2**：兼容Fiber v2
- **v3**：兼容Fiber v3

## 安装

```bash
# 对于Fiber v3
 go get github.com/gofiber/contrib/spnego/v3

# 对于Fiber v2
 go get github.com/gofiber/contrib/spnego/v2
```

## 使用方法

### 对于Fiber v3

```go
package main

import (
    flog "github.com/gofiber/fiber/v3/log"
    "fmt"
    "log"

    "github.com/jcmturner/gokrb5/v8/keytab"
    "github.com/gofiber/fiber/v3"
    "github.com/gofiber/contrib/spnego/v3"
)

func main() {
    app := fiber.New()

    // 创建带有keytab查找函数的配置
    cfg := &v3.Config{
        // 使用函数从文件查找keytab
        KeytabLookup: func() (*keytab.Keytab, error) {
            // 在此实现您的keytab查找逻辑
            // 可以从文件、数据库或其他来源获取
            kt, err := v3.NewKeytabFileLookupFunc("/path/to/keytab/file.keytab")
            if err != nil {
                return nil, err
            }
            return kt()
        },
        // 可选：设置自定义日志器
        Log: flog.DefaultLogger().Logger().(*log.Logger),
    }

    // 创建中间件
    authMiddleware, err := v3.NewSpnegoKrb5AuthenticateMiddleware(cfg)
    if err != nil {
        flog.Fatalf("创建中间件失败: %v", err)
    }

    // 将中间件应用于受保护的路由
    app.Use("/protected", authMiddleware)

    // 访问认证身份
    app.Get("/protected/resource", func(c fiber.Ctx) error {
        identity, ok := v3.GetAuthenticatedIdentityFromContext(c)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).SendString("未授权")
        }
        return c.SendString(fmt.Sprintf("你好, %s!", identity.UserName()))
    })

    app.Listen(":3000")
}
```

### 对于Fiber v2

```go
package main

import (
    "fmt"
    "log"
    "os"

    "github.com/jcmturner/gokrb5/v8/keytab"
    "github.com/gofiber/fiber/v2"
    "github.com/gofiber/contrib/spnego/v2"
)

func main() {
    app := fiber.New()

    // 创建带有keytab查找函数的配置
    cfg := &v2.Config{
        // 使用函数从文件查找keytab
        KeytabLookup: func() (*keytab.Keytab, error) {
            // 在此实现您的keytab查找逻辑
            // 可以从文件、数据库或其他来源获取
            kt, err := v2.NewKeytabFileLookupFunc("/path/to/keytab/file.keytab")
            if err != nil {
                return nil, err
            }
            return kt()
        },
        // 可选：设置自定义日志器
        Log: log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds),
    }

    // 创建中间件
    authMiddleware, err := v2.NewSpnegoKrb5AuthenticateMiddleware(cfg)
    if err != nil {
        log.Fatalf("创建中间件失败: %v", err)
    }

    // 将中间件应用于受保护的路由
    app.Use("/protected", authMiddleware)

    // 访问认证身份
    app.Get("/protected/resource", func(c *fiber.Ctx) error {
        identity, ok := v2.GetAuthenticatedIdentityFromContext(c)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).SendString("未授权")
        }
        return c.SendString(fmt.Sprintf("你好, %s!", identity.UserName()))
    })

    app.Listen(":3000")
}
```

## 动态Keytab查找

该中间件设计具有可扩展性，允许从静态文件以外的各种来源检索keytab：

```go
// 示例：从数据库检索keytab
func dbKeytabLookup() (*keytab.Keytab, error) {
    // 此处实现数据库查找逻辑
    // ...
    return keytabFromDatabase, nil
}

// 示例：从远程服务检索keytab
func remoteKeytabLookup() (*keytab.Keytab, error) {
    // 此处实现远程服务调用逻辑
    // ...
    return keytabFromRemote, nil
}
```

## API 参考

### `NewSpnegoKrb5AuthenticateMiddleware(cfg *Config) (fiber.Handler, error)`

创建一个新的SPNEGO认证中间件。

### `GetAuthenticatedIdentityFromContext(ctx fiber.Ctx) (goidentity.Identity, bool)`

从Fiber上下文中检索已认证的身份。

### `NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error)`

创建一个加载keytab文件的KeytabLookupFunc。

## 配置

`Config`结构体支持以下字段：

- `KeytabLookup`: 检索keytab的函数（必需）
- `Log`: 用于中间件日志记录的日志器（可选，默认为Fiber的默认日志器）

## 要求

- Go 1.21或更高版本
- 对于v3：Fiber v3
- 对于v2：Fiber v2
- Kerberos基础设施

## 注意事项

- 确保您的Kerberos基础设施已正确配置
- 中间件处理SPNEGO协商过程
- 已认证的身份使用`contextKeyOfIdentity`存储在Fiber上下文中
