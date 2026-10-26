package com.lecywza.mytun;

import android.Manifest;
import android.annotation.SuppressLint;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.view.KeyEvent;
import android.view.View;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.TextView;

import androidx.appcompat.app.AppCompatActivity;
import androidx.core.view.ViewCompat;
import androidx.core.view.WindowInsetsCompat;
import androidx.core.app.ActivityCompat;

/**
 * Thin WebView shell around the MyTun console.
 *
 * The heavy lifting (tunnels, proxy, SQLite) lives in the bundled Go core; this
 * activity only starts the foreground service, waits for 127.0.0.1:8899 and
 * shows the very same console a desktop user sees.
 */
public class MainActivity extends AppCompatActivity {
    private WebView web;
    private ProgressBar progress;
    private TextView splash;
    private final Handler ui = new Handler(Looper.getMainLooper());
    private boolean loaded = false;

    @SuppressLint("SetJavaScriptEnabled")
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(0xFF0F1117);

        splash = new TextView(this);
        splash.setText("正在启动 MyTun…");
        splash.setTextColor(0xFF94A3B8);
        splash.setTextSize(14);
        splash.setPadding(48, 200, 48, 0);
        splash.setTextAlignment(View.TEXT_ALIGNMENT_CENTER);
        root.addView(splash, new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.MATCH_PARENT));

        progress = new ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal);
        progress.setIndeterminate(true);
        progress.setVisibility(View.GONE);
        root.addView(progress, new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, 8));

        web = new WebView(this);
        web.setVisibility(View.GONE);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setDatabaseEnabled(true);
        s.setAllowFileAccess(false);
        s.setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
        s.setCacheMode(WebSettings.LOAD_DEFAULT);
        s.setUserAgentString(s.getUserAgentString() + " MyTunAndroid/0.1");
        web.setWebViewClient(new WebViewClient());
        web.setWebChromeClient(new WebChromeClient());
        FrameLayout holder = new FrameLayout(this);
        holder.addView(web, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT));
        root.addView(holder, new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f));

        setContentView(root);

        // Android 15 forces edge-to-edge, so the WebView would otherwise draw
        // under the status bar and the gesture bar. Padding the root view is far
        // more reliable than relying on env(safe-area-inset-*) inside WebView.
        final View shell = root;
        ViewCompat.setOnApplyWindowInsetsListener(shell, (v, insets) -> {
            androidx.core.graphics.Insets bars = insets.getInsets(
                    WindowInsetsCompat.Type.systemBars() | WindowInsetsCompat.Type.displayCutout());
            int top = bars.top;
            if (top == 0) {
                top = getStatusBarHeightPx();
            }
            v.setPadding(bars.left, top, bars.right, bars.bottom);
            return insets;
        });

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                && ActivityCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            ActivityCompat.requestPermissions(this, new String[]{Manifest.permission.POST_NOTIFICATIONS}, 1);
        }

        startCore();
        waitForCore(0);
    }

    private int getStatusBarHeightPx() {
        int id = getResources().getIdentifier("status_bar_height", "dimen", "android");
        if (id > 0) {
            return getResources().getDimensionPixelSize(id);
        }
        return (int) (24 * getResources().getDisplayMetrics().density);
    }

    private void startCore() {
        Intent svc = new Intent(this, TunnelService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(svc);
        } else {
            startService(svc);
        }
    }

    private void waitForCore(int attempt) {
        if (isFinishing()) {
            return;
        }
        new Thread(() -> {
            boolean ready = TunnelService.portReady(TunnelService.PORT, 400);
            ui.post(() -> {
                if (ready) {
                    showConsole();
                } else if (attempt < 120) {
                    splash.setText(attempt < 20 ? "正在启动核心…" : "核心启动较慢，请稍候…");
                    ui.postDelayed(() -> waitForCore(attempt + 1), 500);
                } else {
                    splash.setText("核心启动超时，请下拉退出后重试");
                }
            });
        }, "mytun-probe").start();
    }

    private void showConsole() {
        if (loaded) {
            return;
        }
        loaded = true;
        splash.setVisibility(View.GONE);
        progress.setVisibility(View.GONE);
        web.setVisibility(View.VISIBLE);
        web.loadUrl("http://127.0.0.1:" + TunnelService.PORT + "/");
    }

    @Override
    public boolean onKeyDown(int keyCode, KeyEvent event) {
        if (keyCode == KeyEvent.KEYCODE_BACK && web != null && web.canGoBack()) {
            web.goBack();
            return true;
        }
        return super.onKeyDown(keyCode, event);
    }

    @Override
    protected void onDestroy() {
        if (web != null) {
            web.destroy();
            web = null;
        }
        super.onDestroy();
    }
}
