// amem plugin for OpenCode: forwards session events to `amem hook opencode` and injects
// recalled memory into the system prompt. Managed by `amem install opencode`.
import type { Plugin } from "@opencode-ai/plugin"

const BIN = "__AMEM_BIN__"

async function call(payload: Record<string, unknown>): Promise<string> {
  try {
    const proc = Bun.spawn([BIN, "hook", "opencode"], { stdin: "pipe", stdout: "pipe", stderr: "ignore" })
    proc.stdin.write(JSON.stringify(payload))
    proc.stdin.end()
    const out = (await new Response(proc.stdout).text()).trim()
    if (!out) return ""
    return JSON.parse(out).context ?? ""
  } catch {
    return ""
  }
}

export const AmemPlugin: Plugin = async ({ directory }) => {
  const pending = new Map<string, string>()
  const lastAssistant = new Map<string, string>()
  const queue = (sid: string, ctx: string) => {
    if (ctx) pending.set(sid, (pending.get(sid) ?? "") + ctx + "\n")
  }
  return {
    "chat.message": async (_input: any, output: any) => {
      const sid = output?.message?.sessionID
      if (!sid) return
      const text = (output.parts ?? []).filter((p: any) => p.type === "text").map((p: any) => p.text).join("\n")
      if (output.message.role === "assistant") {
        lastAssistant.set(sid, text)
        return
      }
      queue(sid, await call({ kind: "prompt", session_id: sid, cwd: directory, prompt: text }))
    },
    "tool.execute.after": async (input: any, output: any) => {
      queue(input.sessionID, await call({
        kind: "tool_post", session_id: input.sessionID, cwd: directory, tool: input.tool,
        args: input.args ?? output?.metadata?.args, output: output?.output, error: output?.metadata?.error,
      }))
    },
    "experimental.chat.system.transform": async (input: any, output: any) => {
      const ctx = pending.get(input?.sessionID)
      if (ctx) {
        output.system.push(ctx)
        pending.delete(input.sessionID)
      }
    },
    event: async ({ event }: any) => {
      const sid = event?.properties?.sessionID ?? event?.properties?.info?.id
      if (!sid) return
      switch (event.type) {
        case "session.created":
          queue(sid, await call({ kind: "session_start", session_id: sid, cwd: directory }))
          break
        case "session.idle":
          await call({ kind: "turn_end", session_id: sid, cwd: directory, assistant: lastAssistant.get(sid) ?? "" })
          break
        case "session.compacted":
          queue(sid, await call({ kind: "compact", session_id: sid, cwd: directory }))
          break
        case "session.deleted":
          await call({ kind: "session_end", session_id: sid, cwd: directory })
          break
      }
    },
  }
}
