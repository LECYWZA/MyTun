package com.lecywza.mytun;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.Intent;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.io.RandomAccessFile;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

/**
 * Runs the bundled MyTun core (a static Go binary shipped in assets) as a
 * child process inside a foreground service.
 *
 * Why not JNI/gomobile? The Go core is a complete program that serves the web
 * console and owns every listener. Exec'ing it keeps the desktop and Android
 * builds byte-identical, and the WebView UI is the same console.
 */
public class TunnelService extends Service {
    private static final String TAG = "MyTunSvc";
    private static final String CHANNEL_ID = "mytun-core";
    private static final int NOTIF_ID = 4101;
    public static final int PORT = 8899;

    private Process process;
    private Thread stdoutThread;
    private Thread stderrThread;
    private volatile boolean stopping = false;

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        startForeground(NOTIF_ID, buildNotification("正在启动核心…"));
        if (process == null || !process.isAlive()) {
            new Thread(this::startCore, "mytun-start").start();
        }
        return START_STICKY;
    }

    private void startCore() {
        try {
            String abi = Build.SUPPORTED_ABIS.length > 0 ? Build.SUPPORTED_ABIS[0] : "arm64-v8a";
            String assetName;
            if (abi.contains("arm64")) {
                assetName = "mytun-core-arm64";
            } else if (abi.contains("armeabi") || abi.contains("arm")) {
                assetName = "mytun-core-arm";
            } else {
                assetName = "mytun-core-x86_64";
            }

            File bin = new File(getFilesDir(), "mytun-core");
            File versionFile = new File(getFilesDir(), ".core-version");
            String wantVersion = buildStamp(assetName);
            boolean needCopy = !bin.exists();
            if (!needCopy && versionFile.exists()) {
                String have = readAll(versionFile).trim();
                needCopy = !have.equals(wantVersion);
            }
            if (needCopy) {
                copyAsset(assetName, bin);
                writeAll(versionFile, wantVersion);
            }
            if (!bin.canExecute()) {
                //noinspection ResultOfMethodCallIgnored
                bin.setExecutable(true, false);
            }

            File dataDir = new File(getFilesDir(), "data");
            if (!dataDir.exists() && !dataDir.mkdirs()) {
                Log.w(TAG, "cannot create data dir");
            }

            List<String> cmd = new ArrayList<>();
            cmd.add(bin.getAbsolutePath());
            cmd.add("--data");
            cmd.add(dataDir.getAbsolutePath());
            cmd.add("--host");
            cmd.add("127.0.0.1");
            cmd.add("--port");
            cmd.add(String.valueOf(PORT));
            String dns = activeDNSServer();
            if (dns != null) {
                cmd.add("--dns");
                cmd.add(dns);
            }

            ProcessBuilder pb = new ProcessBuilder(cmd);
            pb.directory(getFilesDir());
            pb.redirectErrorStream(false);
            process = pb.start();
            pump(process.getInputStream(), "core-out");
            pump(process.getErrorStream(), "core-err");
            updateNotification("隧道服务已就绪 · 端口 " + PORT);
            Log.i(TAG, "core started: " + process);
        } catch (Exception e) {
            Log.e(TAG, "failed to start core", e);
            updateNotification("核心启动失败: " + e.getMessage());
        }
    }

    /** Android has no resolv.conf, so hand the core the live network resolver. */
    @SuppressWarnings("deprecation")
    private String activeDNSServer() {
        try {
            android.net.ConnectivityManager cm =
                    (android.net.ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
            if (cm == null) {
                return null;
            }
            android.net.Network net = Build.VERSION.SDK_INT >= Build.VERSION_CODES.M
                    ? cm.getActiveNetwork() : null;
            if (net != null) {
                android.net.LinkProperties lp = cm.getLinkProperties(net);
                if (lp != null) {
                    for (java.net.InetAddress a : lp.getDnsServers()) {
                        if (a != null && !a.isLoopbackAddress()) {
                            return a.getHostAddress();
                        }
                    }
                }
            }
            // Fallback for old devices: read the legacy property.
            Class<?> sp = Class.forName("android.os.SystemProperties");
            Object v = sp.getMethod("get", String.class, String.class).invoke(null, "net.dns1", "");
            String s = String.valueOf(v);
            return s.isEmpty() ? null : s;
        } catch (Throwable t) {
            Log.w(TAG, "dns detect failed: " + t);
            return null;
        }
    }

    private String buildStamp(String assetName) {
        try {
            return assetName + "-" + getPackageManager()
                    .getPackageInfo(getPackageName(), 0).versionName;
        } catch (Exception e) {
            return assetName;
        }
    }

    private void copyAsset(String name, File dest) throws IOException {
        InputStream in = getAssets().open(name);
        OutputStream out = new FileOutputStream(dest);
        byte[] buf = new byte[64 * 1024];
        int n;
        while ((n = in.read(buf)) > 0) {
            out.write(buf, 0, n);
        }
        out.close();
        in.close();
        //noinspection ResultOfMethodCallIgnored
        dest.setReadable(true, false);
        //noinspection ResultOfMethodCallIgnored
        dest.setExecutable(true, false);
    }

    private void pump(InputStream in, String tag) {
        Thread t = new Thread(() -> {
            try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.d(TAG, tag + ": " + line);
                }
            } catch (IOException ignored) {
            }
        }, tag);
        t.setDaemon(true);
        t.start();
        if ("core-err".equals(tag)) {
            stderrThread = t;
        } else {
            stdoutThread = t;
        }
    }

    static String readAll(File f) throws IOException {
        try (RandomAccessFile raf = new RandomAccessFile(f, "r")) {
            byte[] b = new byte[(int) raf.length()];
            raf.readFully(b);
            return new String(b);
        }
    }

    static void writeAll(File f, String s) throws IOException {
        try (FileOutputStream out = new FileOutputStream(f)) {
            out.write(s.getBytes());
        }
    }

    private void createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return;
        }
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (nm == null) {
            return;
        }
        NotificationChannel ch = new NotificationChannel(CHANNEL_ID, getString(R.string.notif_channel),
                NotificationManager.IMPORTANCE_LOW);
        ch.setDescription("Keeps the tunnel core alive in the background");
        nm.createNotificationChannel(ch);
    }

    private Notification buildNotification(String text) {
        Notification.Builder b;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            b = new Notification.Builder(this, CHANNEL_ID);
        } else {
            b = new Notification.Builder(this);
        }
        Intent open = new Intent(this, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP);
        int pendingFlags = Build.VERSION.SDK_INT >= Build.VERSION_CODES.M
                ? android.app.PendingIntent.FLAG_IMMUTABLE : 0;
        android.app.PendingIntent pi = android.app.PendingIntent.getActivity(this, 0, open, pendingFlags);
        return b.setContentTitle(getString(R.string.notif_title))
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_sys_upload_done)
                .setOngoing(true)
                .setContentIntent(pi)
                .build();
    }

    private void updateNotification(String text) {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm != null) {
            nm.notify(NOTIF_ID, buildNotification(text));
        }
    }

    boolean isCoreAlive() {
        return process != null && process.isAlive();
    }

    @Override
    public void onDestroy() {
        stopping = true;
        if (process != null) {
            process.destroy();
            try {
                if (!process.waitFor(3, TimeUnit.SECONDS)) {
                    process.destroyForcibly();
                }
            } catch (InterruptedException ignored) {
                process.destroyForcibly();
            }
            process = null;
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    /** Quick TCP probe used by the activity to know when the console is up. */
    static boolean portReady(int port, int timeoutMs) {
        try (java.net.Socket s = new java.net.Socket()) {
            s.connect(new java.net.InetSocketAddress("127.0.0.1", port), timeoutMs);
            return true;
        } catch (IOException e) {
            return false;
        }
    }
}
