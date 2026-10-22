/* sandbox_stub.c —— macOS 上第一版没有沙箱，见 sandbox.h
 *
 * 没有 Landlock 的执行器走现有的“无沙箱”规则：自动模式下的 Shell 也要审批，
 * Write / Edit 的路径由执行器自己检查（工作目录内可写，外面要 elevated）。
 */
#include "sandbox.h"

int sb_landlock_abi(void)
{
    return 0;
}

int sb_apply(const sb_rules *r, char *err, size_t errlen)
{
    (void)r;
    (void)err;
    (void)errlen;
    return 1;
}
