import { useState } from "react";
import { beforeEach, describe, expect, it } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CoreProvider } from "../../core/context.tsx";
import { FakeClient } from "../../test/fake-client.ts";
import { account, ME } from "../../test/fixtures.ts";
import { CommandHost, clearCommandDraftMemory } from "./CommandHost.tsx";
import { CommandProvider } from "./CommandContext.tsx";
import { Shell } from "../Shell.tsx";
import { replaceHistoryState } from "../../router.ts";
import type {
  CommandExecutionContext,
  CommandProviderHandlers,
  CommandSuggestionProvider,
} from "./command-types.ts";
import type {
  CommunityAccess,
  MemberSummary,
  RoleSummary,
} from "@dilla/client-core";
const CID = "a1".repeat(16),
  CH = "c3".repeat(16),
  OTHER = "d4".repeat(16),
  USER = "33".repeat(16),
  BOB = "44".repeat(16);
beforeEach(() => clearCommandDraftMemory());
interface HostProps {
  fake: FakeClient;
  self?: string;
  draft?: string;
  channel?: string;
  handlers?: CommandProviderHandlers;
  suggest?: CommandSuggestionProvider;
}
function Host({
  fake,
  self = USER,
  draft = "",
  channel = CH,
  handlers,
  suggest,
}: HostProps) {
  const [text, setText] = useState(draft);
  return (
    <CoreProvider client={fake}>
      <CommandProvider handlers={handlers} suggest={suggest}>
        <CommandHost
          communityId={CID}
          channelId={channel}
          bits={0x1ffffff}
          items={[]}
          selfId={self}
          draft={text}
          onDraft={setText}
          onSendLiteral={() => {}}
          encodeMessage={(s) => s}
          openGif={() => {}}
          gifsEnabled
        />
      </CommandProvider>
    </CoreProvider>
  );
}
describe("Task4 fix1 focused lifecycle probes", () => {
  it("does not restore private command text into a different client/account", () => {
    const v = render(
      <Host fake={new FakeClient()} draft="/me private-token" />,
    );
    expect(screen.getByRole("combobox", { name: "text" })).toHaveValue(
      "private-token",
    );
    v.unmount();
    render(<Host fake={new FakeClient()} self={BOB} />);
    expect(screen.queryByRole("combobox", { name: "text" })).toBeNull();
  });
  it("invalidates a live execution when the client or self changes at the same conversation", async () => {
    let captured: CommandExecutionContext | undefined;
    const user = userEvent.setup();
    const handlers: CommandProviderHandlers = {
      me: (_invocation, c) => {
        captured = c;
        return Promise.resolve({
          status: "uncertain",
          private: true,
          message: "pending",
        });
      },
    };
    const v = render(
      <Host fake={new FakeClient()} handlers={handlers} draft="/me hello" />,
    );
    await user.click(screen.getByRole("button", { name: /run/i }));
    v.rerender(<Host fake={new FakeClient()} self={BOB} handlers={handlers} />);
    expect(captured?.isCurrent()).toBe(false);
  });
  it("clears active command text when the signed-in member changes on the same client", () => {
    const fake = new FakeClient();
    const v = render(<Host fake={fake} draft="/me private-token" />);
    expect(screen.getByRole("combobox", { name: "text" })).toHaveValue(
      "private-token",
    );
    v.rerender(<Host fake={fake} self={BOB} />);
    expect(capturedText()).toBeNull();
    function capturedText() {
      return screen.queryByDisplayValue("private-token");
    }
  });
  it("restores a selected entity and retries the same uncertain invocation without reselecting it", async () => {
    const fake = new FakeClient();
    fake.set(`members:${CID}`, [
      { userId: BOB, username: "bob", display: "", kind: 0, roleIds: [] },
    ]);
    const calls: { invocationId: string; requestId: string }[] = [];
    const handlers: CommandProviderHandlers = {
      dm: (invocation, c) => {
        calls.push({
          invocationId: invocation.args.user,
          requestId: c.requestId,
        });
        return Promise.resolve({
          status: "uncertain",
          private: true,
          message: "pending",
        });
      },
    };
    const user = userEvent.setup();
    const v = render(<Host fake={fake} draft="/dm" handlers={handlers} />);
    await user.type(screen.getByRole("combobox", { name: "user" }), "bob");
    await user.click(await screen.findByRole("option", { name: "bob" }));
    await user.click(screen.getByRole("button", { name: /run/i }));
    await screen.findByText("pending");
    v.unmount();
    render(<Host fake={fake} handlers={handlers} />);
    await user.click(screen.getByRole("button", { name: /run/i }));
    expect(calls).toHaveLength(2);
    expect(calls[1]?.invocationId).toBe(BOB);
    expect(calls[1]?.requestId).toBe(calls[0]?.requestId);
  });
  it("does not resurrect a literal command after its explicit publication", async () => {
    const fake = new FakeClient();
    const user = userEvent.setup();
    const v = render(<Host fake={fake} draft="/unknown-private" />);
    await user.click(screen.getByRole("button", { name: /send as text/i }));
    v.unmount();
    render(<Host fake={fake} />);
    expect(
      screen.queryByRole("region", { name: "Command composer" }),
    ).toBeNull();
    expect(document.querySelector(".d-command-composer")).toBeNull();
  });
  it("clears a restored ordinary draft when sent without editing after channel return", async () => {
    replaceHistoryState(null, "", `/c/${CID}/${CH}`);
    const fake = new FakeClient();
    fake.set("account", account());
    fake.set("connection", { status: "online", generation: "7" });
    fake.set("communities", [
      { id: CID, name: "Town", owner: "ee".repeat(16) },
    ]);
    fake.set(`channels:${CID}`, [
      {
        id: CH,
        communityId: CID,
        kind: 0,
        mode: 0,
        name: "general",
        topic: "",
        parentId: null,
        position: 0,
        slowmode: 0,
        announcement: false,
        group: "active",
      },
      {
        id: OTHER,
        communityId: CID,
        kind: 0,
        mode: 0,
        name: "random",
        topic: "",
        parentId: null,
        position: 1,
        slowmode: 0,
        announcement: false,
        group: "active",
      },
    ]);
    fake.set(`afk:${CID}`, {
      channelId: null,
      timeoutSeconds: 300,
      pending: false,
    });
    fake.set(`members:${CID}`, [
      { userId: ME.id, username: "ada", display: "", kind: 0, roleIds: [] },
    ]);
    for (const id of [CH, OTHER])
      fake.set(`timeline:${id}`, {
        channelId: id,
        group: "active",
        items: [],
        hasEarlier: false,
      });
    const user = userEvent.setup();
    render(
      <CoreProvider client={fake}>
        <Shell />
      </CoreProvider>,
    );
    await user.type(
      screen.getByRole("textbox", { name: "message #general" }),
      "A draft",
    );
    await user.click(screen.getByRole("button", { name: "random" }));
    await user.type(
      screen.getByRole("textbox", { name: "message #random" }),
      "B draft",
    );
    await user.click(screen.getByRole("button", { name: "general" }));
    await user.click(screen.getByRole("button", { name: "send" }));
    expect(fake.callsOf("send")).toHaveLength(1);
    expect(
      screen.getByRole("textbox", { name: "message #general" }),
    ).toHaveValue("");
  });
  it("restores the failed send into a returned channel draft without overwriting another draft", async () => {
    replaceHistoryState(null, "", `/c/${CID}/${CH}`);
    const fake = new FakeClient();
    fake.set("account", account());
    fake.set("connection", { status: "online", generation: "7" });
    fake.set("communities", [
      { id: CID, name: "Town", owner: "ee".repeat(16) },
    ]);
    fake.set(`channels:${CID}`, [
      {
        id: CH,
        communityId: CID,
        kind: 0,
        mode: 0,
        name: "general",
        topic: "",
        parentId: null,
        position: 0,
        slowmode: 0,
        announcement: false,
        group: "active",
      },
      {
        id: OTHER,
        communityId: CID,
        kind: 0,
        mode: 0,
        name: "random",
        topic: "",
        parentId: null,
        position: 1,
        slowmode: 0,
        announcement: false,
        group: "active",
      },
    ]);
    fake.set(`afk:${CID}`, {
      channelId: null,
      timeoutSeconds: 300,
      pending: false,
    });
    fake.set(`members:${CID}`, [
      { userId: ME.id, username: "ada", display: "", kind: 0, roleIds: [] },
    ]);
    for (const id of [CH, OTHER])
      fake.set(`timeline:${id}`, {
        channelId: id,
        group: "active",
        items: [],
        hasEarlier: false,
      });
    fake.handler = (c) => {
      if (c.m === "send") return Promise.reject(new Error("refused"));
      return Promise.resolve(null);
    };
    const user = userEvent.setup();
    render(
      <CoreProvider client={fake}>
        <Shell />
      </CoreProvider>,
    );
    await user.type(
      screen.getByRole("textbox", { name: "message #general" }),
      "A draft",
    );
    await user.click(screen.getByRole("button", { name: "random" }));
    await user.type(
      screen.getByRole("textbox", { name: "message #random" }),
      "B draft",
    );
    await user.click(screen.getByRole("button", { name: "general" }));
    await user.click(screen.getByRole("button", { name: "send" }));
    await waitFor(() =>
      expect(
        screen.getByRole("textbox", { name: "message #general" }),
      ).toHaveValue("A draft"),
    );
    await user.click(screen.getByRole("button", { name: "random" }));
    expect(
      screen.getByRole("textbox", { name: "message #random" }),
    ).toHaveValue("B draft");
  });
  it("does not open stale unban confirmation after its preload resolves in another context", async () => {
    let resolveLoad!: (value: unknown) => void;
    const fake = new FakeClient();
    fake.set(`permissions:${CID}`, {
      loaded: true,
      top: 4,
      owner: false,
      manageCommunity: false,
      kickMembers: false,
      banMembers: true,
      moderateMembers: false,
      manageMessages: false,
      manageRoles: false,
      manageChannels: false,
      createInvite: false,
      everyoneId: "88".repeat(16),
      overwritesLoaded: true,
    } satisfies CommunityAccess);
    fake.set(`bans:${CID}`, [
      {
        userId: BOB,
        name: "bob",
        reason: "",
        byUser: USER,
        created: 1,
        expires: null,
      },
    ]);
    fake.handler = (c) =>
      c.m === "loadBans"
        ? new Promise<unknown>((resolve) => {
            resolveLoad = resolve;
          })
        : Promise.resolve(null);
    const suggest: CommandSuggestionProvider = () =>
      Promise.resolve([{ id: BOB, value: BOB, label: "bob" }]);
    function H({ channel, draft }: { channel: string; draft: string }) {
      return (
        <CoreProvider client={fake}>
          <CommandProvider suggest={suggest}>
            <CommandHost
              communityId={CID}
              channelId={channel}
              bits={0x1ffffff}
              items={[]}
              selfId={USER}
              draft={draft}
              onDraft={() => {}}
              onSendLiteral={() => {}}
              encodeMessage={(s) => s}
              openGif={() => {}}
              gifsEnabled
            />
          </CommandProvider>
        </CoreProvider>
      );
    }
    const user = userEvent.setup();
    const v = render(<H channel={CH} draft="/unban" />);
    await user.type(screen.getByRole("combobox", { name: "user" }), "bob");
    await user.click(await screen.findByRole("option", { name: "bob" }));
    await user.click(screen.getByRole("button", { name: /run/i }));
    v.rerender(<H channel={OTHER} draft="/me new" />);
    await screen.findByRole("combobox", { name: "text" });
    act(() => resolveLoad(null));
    expect(screen.queryByRole("dialog", { name: "Unban bob?" })).toBeNull();
  });
  it("keeps the same request identity after an ambiguous warning transport failure", async () => {
    const fake = new FakeClient(),
      ids: string[] = [];
    fake.set("communities", [
      { id: CID, name: "Town", owner: "ee".repeat(16) },
    ]);
    fake.set(`permissions:${CID}`, {
      loaded: true,
      manageCommunity: false,
      kickMembers: false,
      banMembers: false,
      moderateMembers: true,
      top: 4,
      owner: false,
      manageMessages: false,
      manageRoles: false,
      manageChannels: false,
      createInvite: false,
      everyoneId: "88".repeat(16),
      overwritesLoaded: true,
    } satisfies CommunityAccess);
    const roleSelf = "a2".repeat(16),
      roleBob = "a3".repeat(16);
    const role = (id: string, position: number): RoleSummary => ({
      id,
      name: "role",
      position,
      allow: 0,
      deny: 0,
      color: 0,
      hoist: false,
      mentionable: false,
      members: 1,
    });
    fake.set(`roles:${CID}`, [role(roleSelf, 4), role(roleBob, 1)]);
    fake.set(`members:${CID}`, [
      {
        userId: USER,
        username: "self",
        display: "",
        kind: 0,
        roleIds: [roleSelf],
      },
      {
        userId: BOB,
        username: "bob",
        display: "",
        kind: 0,
        roleIds: [roleBob],
      },
    ] satisfies MemberSummary[]);
    fake.handler = (c) => {
      if (c.m === "warnMember") {
        ids.push(c.requestId);
        return Promise.reject(
          Object.assign(new Error("transport lost after acceptance"), {
            code: "E_NETWORK",
          }),
        );
      }
      return Promise.resolve(null);
    };
    const user = userEvent.setup();
    render(<Host fake={fake} draft="/warn user:bob reason:spam" />);
    await user.click(await screen.findByRole("option", { name: "bob" }));
    for (let n = 0; n < 2; n++) {
      await user.click(screen.getByRole("button", { name: /run/i }));
      await user.click(await screen.findByRole("button", { name: "Continue" }));
      await screen.findByText(/result is uncertain \(E_NETWORK\)/);
    }
    expect(ids).toHaveLength(2);
    expect(ids[1]).toBe(ids[0]);
  });
});
