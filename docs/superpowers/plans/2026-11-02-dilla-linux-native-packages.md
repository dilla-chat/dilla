# Native Linux distributions implementation plan

**2026-11-02 root ruling:** The user requested DEB/RPM/AUR and full release support and challenged custom complexity against electron-builder documentation. The standard-native scope in the [revised design](../specs/2026-11-02-dilla-linux-native-packages-design.md) and `.superpowers/sdd/2026-11-02-dilla-linux-native-packages/task-2-lifecycle-subdesign.md` supersedes the earlier native lifecycle/admission plan. AppImage and Windows/macOS work remains unchanged.

1. Retain build-owned `system-package` authority and check-only native updater policy; remove native startup admission, fixed receipt/floor and addon requirements. Preserve sandbox, signed release checks, quit guards and profile identity. Test spoofed inputs and forbidden update/setup roles.
2. Build stock electron-builder DEB/RPM in an isolated native build; create pinned AUR recipe and `.SRCINFO`. Keep AppImage packaging transforms off the native path. Inspect final package scripts, layout, modes, dependencies and registrations; bind exact outputs to signed release inventory and provenance.
3. Retire the custom admission fixture and CI gate only as that contract is removed. Preserve the original AppImage foundation controls and shared account/call/media coverage. Native publication requires a real installed-package proof, not the retired fixture.
4. On clean Ubuntu 24, Debian 13, Fedora enforcing and pinned Arch, prove ordinary-user sandboxed client and account/keyring/draft continuity through manager install, upgrade, relaunch and remove/reinstall. Observe failed package operation and repair, signature-policy refusal, and package-specific Ubuntu policy. No global policy relaxation, root Electron, sandbox bypass, or unsupported claim of app-managed rollback or external administrator call veto.

Build/inspection alone does not qualify native release. No workstation privileged operation, AUR publication, commit or push is authorized by this plan.
