// Shared types matching amp's frame shapes (subset we care about).

export type Role = "user" | "assistant" | "info";

export interface ContentBlock {
  type: "text" | "tool_use" | "tool_result" | "thinking" | string;
  text?: string;
  // tool_use
  id?: string;
  name?: string;
  input?: any;
  // tool_result
  toolUseID?: string;
  tool_use_id?: string;
  run?: any;
  // thinking
  thinking?: string;
  signature?: string;
  blockState?: string;
  finalTime?: number;
  startTime?: number;
}

export interface Message {
  role: Role;
  content: ContentBlock[];
  messageId?: string | number;
  state?: { type?: string; stopReason?: string };
  usage?: any;
  meta?: { sentAt?: number };
  agentMode?: string;
}

export interface ThreadSummary {
  id: string;
  title?: string;
  agentMode?: string;
  created?: number;
  userLastInteractedAt?: number;
  messageCount?: number;
}

export interface ThreadData {
  id: string;
  title?: string;
  agentMode?: string;
  reasoningEffort?: string;
  messages?: Message[];
  env?: {
    initial?: {
      workingDirectory?: string;
      workspaceRoot?: string;
      trees?: Array<{ uri?: string; displayName?: string; repository?: any }>;
    };
  };
}

export type AgentMode = "smart" | "large" | "rush" | "deep" | "frontier";

// Wire frames from coordinator
export interface ServerFrame {
  type: string;
  reqId?: string;
  message?: Message;
  text?: string;
  blocks?: any[];
  blockIndex?: number;
  state?: string;
  online?: boolean;
  threadId?: string;
  title?: string;
  toolCallId?: string;
  toolName?: string;
  args?: any;
  progress?: any;
  error?: { message?: string };
}
