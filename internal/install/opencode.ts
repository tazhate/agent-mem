// amem plugin for OpenCode: forwards session events to `amem hook opencode` and injects
// recalled memory into the system prompt. Managed by `amem install opencode`.
import type { Plugin } from "@opencode-ai/plugin"

const BIN = __AMEM_BIN__

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
  // OpenCode rebuilds the system prompt for every model request of a turn, so injected memory
  // stays in place until the next user prompt replaces it.
  const turnContext = new Map<string, string[]>()
  const roles = new Map<string, string>() // messageID -> role
  const lastAssistant = new Map<string, string>()

  const add = (sid: string, ctx: string) => {
    if (!ctx) return
    const list = turnContext.get(sid) ?? []
    list.push(ctx)
    turnContext.set(sid, list.slice(-6))
  }

  return {
    "chat.message": async (_input: any, output: any) => {
      // This hook sees the user's message: start a new turn.
      const sid = output?.message?.sessionID
      if (!sid) return
      const text = (output.parts ?? []).filter((p: any) => p.type === "text").map((p: any) => p.text).join("\n")
      turnContext.set(sid, [])
      add(sid, await call({ kind: "prompt", session_id: sid, cwd: directory, prompt: text }))
    },
    "tool.execute.after": async (input: any, output: any) => {
      add(input.sessionID, await call({
        kind: "tool_post", session_id: input.sessionID, cwd: directory, tool: input.tool,
        args: input.args ?? output?.metadata?.args, output: output?.output, error: output?.metadata?.error,
      }))
    },
    "experimental.chat.system.transform": async (input: any, output: any) => {
      const ctx = turnContext.get(input?.sessionID)
      if (ctx?.length) output.system.push(ctx.join("\n"))
    },
    event: async ({ event }: any) => {
      const props = event?.properties ?? {}
      if (event.type === "message.updated" && props.info?.id) {
        roles.set(props.info.id, props.info.role)
        return
      }
      if (event.type === "message.part.updated") {
        const part = props.part
        if (part?.type === "text" && roles.get(part.messageID) === "assistant" && part.sessionID) {
          lastAssistant.set(part.sessionID, part.text ?? "")
        }
        return
      }
      const sid = props.sessionID ?? props.info?.id
      if (!sid) return
      switch (event.type) {
        case "session.created":
          add(sid, await call({ kind: "session_start", session_id: sid, cwd: directory }))
          break
        case "session.idle":
          await call({ kind: "turn_end", session_id: sid, cwd: directory, assistant: lastAssistant.get(sid) ?? "" })
          break
        case "session.compacted":
          add(sid, await call({ kind: "compact", session_id: sid, cwd: directory }))
          break
        case "session.deleted":
          await call({ kind: "session_end", session_id: sid, cwd: directory })
          turnContext.delete(sid)
          lastAssistant.delete(sid)
          break
      }
    },
  }
}
