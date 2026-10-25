/* core.h —— nano-agent 内核状态机
 *
 * 这里只有纯逻辑：不做 I/O、不读时钟、不起线程。宿主把外部发生的事情作为
 * 事件喂进来（na_core_feed），再把内核要求做的事情作为命令取出去
 * （na_core_next）。时间也随事件传入，所以同一串事件永远得到同一串命令，
 * 方便单元测试和回放。
 *
 * 内核保证的不变量：
 *   - 每个模型发起的工具调用恰好得到一个结果（成功、失败、超时、被拒都算），
 *     全部有了结果才会请求下一轮模型；
 *   - 每个 run 一定会以一条 DONE 结束（与网关断开时除外，那时 run 直接丢弃）。
 */
#ifndef NA_CORE_H
#define NA_CORE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* 工具按会不会改动东西分三类，权限模式据此决定直接执行、先审批还是拒绝 */
typedef enum {
    NA_TOOL_PUBLIC_READ, /* 网关执行、权限由网关判断的工具（搜索、记忆、发消息、发文件……），任何模式都交给网关 */
    NA_TOOL_READ,    /* 只读：Read、Grep */
    NA_TOOL_WRITE,   /* 改文件：Write、Edit */
    NA_TOOL_SHELL,   /* 任意命令 */
    NA_TOOL_SCHEDULE, /* 管理员定时任务，按 action 和模式审批 */
    NA_TOOL_ADMIN,   /* 网关管理工具：任何模式都要审批 */
} na_tool_kind;

/* 工具的策略部分。描述和参数 schema 由宿主直接报给网关，内核不关心。 */
typedef struct {
    const char  *name;
    na_tool_kind kind;
    int          timeout_ms;   /* 单次执行的兜底时限 */
} na_tool_policy;

/* 权限模式，由网关按会话设置和触发者身份决定，随 input 传来 */
typedef enum {
    NA_MODE_NONE,      /* 不能用工具（比如群里的非管理员） */
    NA_MODE_READONLY,  /* 只能用只读工具 */
    NA_MODE_ASK,       /* 只读工具直接执行，其余每次先审批 */
    NA_MODE_AUTO,      /* 全部直接执行（都在工作目录的沙箱里） */
} na_mode;

typedef struct {
    const na_tool_policy *tools; size_t ntools;   /* 由宿主持有，生命周期须长于 core */
    bool sandbox;             /* 执行器能套上沙箱；不能时，自动模式下的 Shell 也要审批 */
    /* 下面三项是网关没在 input 里给值时的兜底（kernel.conf 里的值）；
     * 网关现在每一轮都带上自己的设置，以网关为准 */
    int max_steps;            /* 每个 run 最多请求几次模型 */
    int run_timeout_ms;       /* 每个 run 最长持续时间 */
    int approval_timeout_ms;  /* 等待审批的最长时间 */
} na_core_config;

typedef enum {
    NA_EV_INPUT,      /* 会话收到新输入，开启 run：sid, from, mode, policy */
    NA_EV_TURN,       /* 模型完成一轮：sid, reason, calls, detail */
    NA_EV_APPROVE,    /* 审批结果：sid, id, ok（审批人是否有权限由网关判断） */
    NA_EV_CANCEL,     /* 用户要求停止：sid */
    NA_EV_TOOL_DONE,  /* 本地工具执行完毕：sid, id, ok, content */
    NA_EV_TICK,       /* 只推进时间，用来检查超时 */
    NA_EV_RESET,      /* 与网关断开：丢弃所有 run */
} na_ev_type;

typedef enum {
    NA_TURN_FINAL,     /* 模型给出了最终回复 */
    NA_TURN_TOOL_USE,  /* 模型请求调用工具 */
    NA_TURN_REFUSED,   /* 模型拒绝回答 */
    NA_TURN_ERROR,     /* 模型调用失败（网关已经重试过） */
} na_turn_reason;

/* 执行工具的机器有没有沙箱（工作目录可能在别的机器上，和本机不同） */
typedef enum {
    NA_SB_DEFAULT = 0,   /* 用 run 的设置（再没有就用 na_core_config.sandbox） */
    NA_SB_YES,
    NA_SB_NO,
} na_sandbox;

typedef struct {
    const char *id;     /* 调用 ID，由模型生成 */
    const char *name;   /* 工具名 */
    const char *args;   /* 参数 JSON 文本；args_ok 为 false 时是原始文本，只用于报错 */
    bool args_ok;       /* 参数是否为合法的 JSON 对象 */
    na_sandbox sandbox; /* 这个调用在哪台机器上执行，那里有没有沙箱（宿主按参数里的工作目录判断） */
    const char *error;  /* 宿主预先检查出的问题（比如工作目录写错了）：非空时直接以此失败，不执行 */
    bool elevated;      /* 模型要求提权写工作目录外（每次都要审批） */
    const char *action; /* 参数里的 action（宿主读出来）：Schedule 和 GatewayAdmin 按它决定怎么审批 */
} na_call;

typedef struct {
    na_ev_type type;
    const char *sid;
    const char *from;
    na_mode mode;
    /* INPUT：这一轮最多请求几次模型、总时限、审批时限。大于 0 是上限，0 用内核配置，小于 0 表示不限 */
    int max_steps, run_timeout_ms, approval_timeout_ms;
    bool elevate;         /* INPUT：这个身份的会话能不能提权（访客不能） */
    na_sandbox sandbox;   /* INPUT：这一轮的工具在哪里执行，那里有没有沙箱 */
    const char *policy;   /* 工作目录等执行约束，内核不解析，原样交给执行器 */
    na_turn_reason reason;
    const na_call *calls; size_t ncalls;
    const char *detail;
    const char *id;
    bool ok;
    const char *content;
} na_event;

typedef enum {
    /* 发往网关 */
    NA_CMD_MODEL,        /* 请模型基于会话上下文生成下一轮：sid */
    NA_CMD_RESULT,       /* 工具结果：sid, id, ok, text */
    NA_CMD_APPROVE_REQ,  /* 请求管理员审批：sid, id, name, args, from */
    NA_CMD_NOTICE,       /* 给用户看的提示：sid, text */
    NA_CMD_DONE,         /* run 结束：sid, status, text（详情，可为 NULL） */
    /* 发往本地工具执行器 */
    NA_CMD_EXEC,         /* 执行工具：sid, id, name, args, from, policy, timeout_ms, elevated */
    NA_CMD_KILL,         /* 终止执行：sid, id */
} na_cmd_type;

typedef struct na_cmd {
    na_cmd_type type;
    char *sid, *id, *name, *args, *from, *text, *policy;
    const char *status;  /* DONE："ok" "budget" "timeout" "cancelled" "refused" "error" */
    bool ok;
    bool elevated;
    int  timeout_ms;
    struct na_cmd *next;
} na_cmd;

typedef struct na_core na_core;

na_core *na_core_new(const na_core_config *cfg);
void     na_core_free(na_core *c);

/* 喂一个事件。返回 0；事件和当前状态对不上（比如迟到的工具结果）时返回 -1，
 * 该事件被忽略。 */
int      na_core_feed(na_core *c, int64_t now_ms, const na_event *ev);

/* 取出下一条命令，没有则返回 NULL。用完调用 na_cmd_free。 */
na_cmd  *na_core_next(na_core *c);
void     na_cmd_free(na_cmd *cmd);

/* 最近的超时时刻，没有则返回 -1。宿主据此决定 poll 等多久。 */
int64_t  na_core_next_deadline(const na_core *c);
size_t   na_core_active_runs(const na_core *c);
/* 会话正在进行的 run 的执行约束（INPUT 时传入的 policy），没有 run 时返回 NULL */
const char *na_core_policy(const na_core *c, const char *sid);

#endif
