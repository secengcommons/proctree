## Windows

Windows creates a kill-on-close Job Object then creates the target suspended inside it through `PROC_THREAD_ATTRIBUTE_JOB_LIST`. Private inheritable duplicates restrict `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` to standard input, standard output and standard error

The controller attaches its Go process state and closes the parent copies of those duplicates before resuming the primary thread. Closing the controller's final Job handle terminates remaining members

Standard input and output use overlapped local named pipes so cleanup can cancel pending transfers within the shared deadline

Compatible parent Jobs retain Proctree as a nested child Job. Job-list admission failure refuses before target creation

## Unix

The Unix implementation targets Darwin, DragonFly BSD, FreeBSD, illumos, Linux, NetBSD, OpenBSD and Solaris

A watchdog enters a new process group during creation and owns control and readiness pipes. The target joins that group only after watchdog validation and an exact closed readiness frame; pipe closure or controller exit terminates the group

Unexpected watchdog exit is observed during execution and terminates the target group

The guarantee covers descendants remaining in the inherited group. Deliberate group or session escape is excluded from termination but cannot extend output capture beyond the cleanup deadline

## Qualification

Platform | Native tests | Race tests
--- | --- | ---
Windows | Server 2022 x64, Server 2025 x64 and Windows 11 ARM64 | Server 2025 x64
Linux | Ubuntu 22.04 and 24.04 on x64 and ARM64 | Ubuntu 24.04 x64 and ARM64
macOS | Versions 14, 15 and 26 on ARM64; versions 15 and 26 on Intel | Version 15 on ARM64 and Intel
FreeBSD | Versions 13.5, 14.4 and 15.1 on x64; version 15.1 on ARM64 | Version 15.1 on x64
OpenBSD | Versions 7.7, 7.8 and 7.9 on x64; version 7.9 on ARM64 | -
NetBSD | Versions 9.4, 10.1 and 11.0 on x64; version 11.0 on ARM64 | -
DragonFly BSD | Version 6.4.2 on x64 | -
illumos | OmniOS r151058 on x64 | -
Solaris | Version 11.4 on x64 | -
AIX | Compile-only unsupported owner on PPC64 | -
Other Go ports | Compile-only or unsupported | -

Atomic Windows Job-list creation requires Windows 10 or Windows Server 2016 and newer

Unsupported targets refuse execution rather than falling back to immediate-process-only termination. Cross-compilation does not establish runtime support

## Containers

Process-tree ownership is limited to the PID namespace containing the caller. It does not cover the host, another container or a deliberate namespace escape

The Alpine tests expect `cleanup_failure` when Proctree runs as PID 1 without an init process. Ordinary ownership tests run below the container runtime's init process

Container tests use a non-root identity, read-only root, no network, no ambient capabilities and a fixed process limit

Policies which deny process-group creation, signalling or pipe operations cause refusal or failure
