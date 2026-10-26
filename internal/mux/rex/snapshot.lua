-- Every session's view and every tiled block's process, title and program
-- status, from one connection: what Backend.Snapshot otherwise asks for a CLI
-- call at a time.
-- A call into a session needs the connection attached to it, and attaching
-- answers with the view, so each session is attach, probes, detach.
--
-- The active window's focused block is probed too when it isn't tiled (a
-- popup's layer), since that's where a session's path comes from.

local terminal = "com.superlogical.terminal"
local list = assert(rex.call("session.list"))

local sessions = {}
for _, s in ipairs(list.sessions or {}) do
  local entry = { session_id = s.session_id, label = s.label, probes = {} }
  local attached = rex.call("session.attach", { session_id = s.session_id })
  if attached ~= nil and attached.view ~= nil then
    local view = attached.view
    entry.view = view
    local function probe(block_id, with_title)
      local args = { session_id = s.session_id, block_id = block_id }
      local facts = { process = rex.block.call(terminal, "process", args) }
      if with_title then
        facts.title = rex.block.call(terminal, "title", args)
        -- nil on a server too old to have it, like any failed call here.
        facts.status = rex.block.call(terminal, "program_status", args)
      end
      entry.probes[block_id] = facts
    end
    for _, w in ipairs(view.windows or {}) do
      for _, layer in ipairs(w.layers or {}) do
        if layer.kind == "tiled" then
          for _, b in ipairs(layer.blocks or {}) do
            probe(b.block_id, true)
          end
        end
      end
      local focused = w.focused_block_id
      if w.window_id == view.active_window_id and focused and entry.probes[focused] == nil then
        probe(focused, false)
      end
    end
    rex.call("session.detach", { session_id = s.session_id })
  end
  sessions[#sessions + 1] = entry
end
return { sessions = sessions }
