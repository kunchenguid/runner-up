# Changelog

## [0.1.11](https://github.com/kunchenguid/runner-up/compare/v0.1.10...v0.1.11) (2026-11-04)


### Features

* let runner-up update itself from signed releases ([b036f52](https://github.com/kunchenguid/runner-up/commit/b036f52284a1a93002f1262b685f4436ce8503a1))


### Bug Fixes

* **agent:** hand Docker places the registration on standard input ([0a2b350](https://github.com/kunchenguid/runner-up/commit/0a2b350c83d27fef83175dcbfc264b6ae8a58d6f))
* **cli:** give each command its own flags, clean -h exits and clear errors ([6656084](https://github.com/kunchenguid/runner-up/commit/665608447a8d039e1cf8a56eb64cfa3030f41f74))
* **coordinator:** place outside the lock, split the service, guard place.json ([c2c6c6b](https://github.com/kunchenguid/runner-up/commit/c2c6c6b2eae4ca16c30169e24707c380a29fe154))
* **release:** build static binaries and verify install downloads ([25e7a8f](https://github.com/kunchenguid/runner-up/commit/25e7a8f7edb09dec9e579f2206641c179dfba03f))

## [0.1.10](https://github.com/kunchenguid/runner-up/compare/v0.1.9...v0.1.10) (2026-11-03)


### Features

* **coordinator:** apply a changed setup file in place ([dfcc0b3](https://github.com/kunchenguid/runner-up/commit/dfcc0b34cf4f8dbcd846909e5974fa9cea8dcf41))
* **drain:** pause named places until a named pair of jobs ran there ([1fc1804](https://github.com/kunchenguid/runner-up/commit/1fc1804e5123e06a32c4d11d2a8b36f111b04575))

## [0.1.9](https://github.com/kunchenguid/runner-up/compare/v0.1.8...v0.1.9) (2026-11-03)


### Bug Fixes

* **agent:** read tart guests from the guest kernel, within their size ([a44c2b8](https://github.com/kunchenguid/runner-up/commit/a44c2b849cc77b5c011d0272b0f6076f3e06f63b))

## [0.1.8](https://github.com/kunchenguid/runner-up/compare/v0.1.7...v0.1.8) (2026-11-03)


### Bug Fixes

* **agent:** relay a Windows account job's output while its agent is connected ([fab774a](https://github.com/kunchenguid/runner-up/commit/fab774a210fe6b22cc39486ad1bccbf408d5f5cb))
* **coordinator:** remove a spare runner from its scale set before stopping it ([537afe0](https://github.com/kunchenguid/runner-up/commit/537afe027e820c9445fcca6dea529fca96ec2402))

## [0.1.7](https://github.com/kunchenguid/runner-up/compare/v0.1.6...v0.1.7) (2026-11-03)


### Features

* **agent:** support Apple container Linux guests and pool deprioritization ([447ba5e](https://github.com/kunchenguid/runner-up/commit/447ba5e465a5ea062d89320e5d762e9a68c85391))

## [0.1.6](https://github.com/kunchenguid/runner-up/compare/v0.1.5...v0.1.6) (2026-11-02)


### Features

* **sizing:** size jobs by pool and reserve room ([b819642](https://github.com/kunchenguid/runner-up/commit/b819642338bec3231cd5cca51dd2b63d5eb0f40a))

## [0.1.5](https://github.com/kunchenguid/runner-up/compare/v0.1.4...v0.1.5) (2026-11-01)


### Bug Fixes

* **coordinator:** forget waiting jobs of runs GitHub closed ([6a674bc](https://github.com/kunchenguid/runner-up/commit/6a674bc811436c093e73a3146a28403e2e58d066))

## [0.1.4](https://github.com/kunchenguid/runner-up/compare/v0.1.3...v0.1.4) (2026-10-31)


### Bug Fixes

* **agent:** open the service's pipe with only the runner account's grant ([bb91b36](https://github.com/kunchenguid/runner-up/commit/bb91b364f202f9f89cc925a92bca966737adf5dc))
* **coordinator:** remove an idle runner from its scale set before stopping it ([de65e68](https://github.com/kunchenguid/runner-up/commit/de65e68d9429a34dc12fc59a0caa6d911d060848))

## [0.1.3](https://github.com/kunchenguid/runner-up/compare/v0.1.2...v0.1.3) (2026-10-31)


### Features

* **config:** run each pool's jobs on its own image in a shared place ([b013a47](https://github.com/kunchenguid/runner-up/commit/b013a47af19e83e79201f73a9043db477fecac85))


### Bug Fixes

* **coordinator:** rest a machine whose agent cannot be reached ([4bb6e7f](https://github.com/kunchenguid/runner-up/commit/4bb6e7f2fb7f972612f7e5bd3ac43de2a9022828))

## [0.1.2](https://github.com/kunchenguid/runner-up/compare/v0.1.1...v0.1.2) (2026-10-29)


### Features

* **coordinator:** add queued borrowing of tart places ([0bc347d](https://github.com/kunchenguid/runner-up/commit/0bc347d091a780e46f0cee7b74376af193ef1945))

## [0.1.1](https://github.com/kunchenguid/runner-up/compare/v0.1.0...v0.1.1) (2026-10-29)


### Features

* **plan:** put a pool's jobs first with the pool:NAME order rule ([de81f69](https://github.com/kunchenguid/runner-up/commit/de81f691559fe0fc6e54c934766be3598a78dc56))

## 0.1.0 (2026-10-29)


### Features

* first public release ([dc37c00](https://github.com/kunchenguid/runner-up/commit/dc37c0032fdf110d3687af5d29d9f6281688c277))
