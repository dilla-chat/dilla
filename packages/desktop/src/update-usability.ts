/** A local startup gate. Network state and window size are deliberately absent:
 * every stored instance must prove its bundled UI and local core are usable. */
export class StartupUsability {
 private instances = new Map<string, boolean>();
 private sealed = false;
 private manager = false;
 private reported = false;
 private uncertain = false;
 private serial: Promise<void> = Promise.resolve();
 private report: (ready: boolean) => Promise<void>;
 constructor(report: (ready: boolean) => Promise<void>) { this.report = report; }
 expect(instance: string): void { this.instances.set(instance, false); }
 async status(instance: string, usable: boolean): Promise<void> {
  if (!this.instances.has(instance)) return;
  this.instances.set(instance, usable); await this.check();
 }
 async managerUnloaded(): Promise<void> { this.manager = false; await this.check(); }
 async managerLoaded(): Promise<void> { this.manager = true; await this.check(); }
 async seal(): Promise<void> { this.sealed = true; await this.check(); }
 private check(): Promise<void> {
  const update = async () => {
   const ready = this.sealed && (this.instances.size ? [...this.instances.values()].every(Boolean) : this.manager);
   if (ready === this.reported && !this.uncertain) return;
   this.uncertain = true;
   await this.report(ready); this.reported = ready; this.uncertain = false;
  };
  this.serial = this.serial.then(update, update);
  return this.serial;
 }
}
