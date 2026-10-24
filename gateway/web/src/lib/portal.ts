// 把弹窗挪到 body 下面显示：面板本身有层级和滚动，弹窗放在里面会被盖住或者跟着滚
export function portal(node: HTMLElement) {
  document.body.appendChild(node);
  return {
    destroy() {
      node.remove();
    },
  };
}
