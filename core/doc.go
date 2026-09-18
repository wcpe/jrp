// Package core 是 JRP 数据面的公共入口，提供嵌入宿主在代码中构造并校验不可变配置快照的类型化入口（FR-32）。
//
// 包内约定：
//
//   - 只接受宿主在代码中传入的值：不读取配置文件、环境变量或数据库，不提供反序列化入口。
//   - 构建结果是不可变值：字段全部私有，读取集合返回副本，宿主传入的切片在构建时即被复制。
//   - 校验失败返回可判定、可分类的错误值，宿主用 errors.Is(err, ErrConfigInvalid) 与 errors.As 判定；
//     非法配置一律返回错误，绝不 panic。
//   - 公共签名只使用标准库类型与本包的值类型。
//
// 错误模型的遍历契约：校验按固定顺序收集全部问题，返回聚合值 ConfigErrors。
// 宿主用 errors.Is(err, ErrConfigInvalid) 判定校验失败，用 errors.As 取出单条 *ConfigError
// 或完整 ConfigErrors 列表；单条问题的错误码取值见 ErrorCode 常量，字符串匹配不构成稳定契约。
//
// 数量与长度上限由本包导出常量定义（MaxProxyCount、MaxClientCredentialCount、MaxProxyNameLength），
// 时间参数零值采用 DefaultHeartbeat 与 DefaultTimeout；上限不提供宿主可调参数，避免无界分配。
package core
